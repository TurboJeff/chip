// Copyright 2026 Jeff Webb
// SPDX-License-Identifier: Apache-2.0

// Package chip is the chipin chip: the agent that shares local resources with
// a chipin server.
package chip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/TurboJeff/chip/proto"
	"github.com/TurboJeff/chip/tunnel"
)

const maxBody = 32 << 20

type Agent struct {
	cfg       *Config
	resources map[string]*resource
	order     []*resource
	log       *slog.Logger
}

type resource struct {
	Resource
	upstream *url.URL
	proxy    *httputil.ReverseProxy
	sem      chan struct{}
	allowed  map[string]bool
	inflight atomic.Int64
	client   *http.Client
}

func New(cfg *Config, log *slog.Logger) (*Agent, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, resources: map[string]*resource{}, log: log}
	for _, rc := range cfg.Resources {
		u, _ := url.Parse(strings.TrimRight(rc.Upstream, "/"))
		transport := upstreamTransport()
		r := &resource{Resource: rc, upstream: u, client: &http.Client{Timeout: 5 * time.Second, Transport: transport}}
		if rc.MaxConcurrent > 0 {
			r.sem = make(chan struct{}, rc.MaxConcurrent)
		}
		if len(rc.Models) > 0 {
			r.allowed = map[string]bool{}
			for _, m := range rc.Models {
				r.allowed[m] = true
			}
		}
		r.proxy = &httputil.ReverseProxy{
			Transport:     transport,
			Rewrite:       r.rewrite,
			FlushInterval: -1, // stream SSE tokens immediately
			ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
				if req.Context().Err() == nil {
					log.Warn("upstream error", "resource", r.Name, "err", err)
				}
				writeError(w, http.StatusBadGateway, "upstream_error", "the shared resource did not respond")
			},
		}
		a.resources[rc.Name] = r
		a.order = append(a.order, r)
	}
	return a, nil
}

// Run keeps the tunnel up until ctx is cancelled, reconnecting with backoff.
func (a *Agent) Run(ctx context.Context) error {
	if u, err := url.Parse(a.cfg.Server); err == nil && u.Scheme == "http" && !isLocal(u.Hostname()) {
		a.log.Warn("server URL is not https; traffic to the server is unencrypted", "server", a.cfg.Server)
	}
	backoff := time.Second
	for {
		sess, err := tunnel.Dial(ctx, a.cfg.Server, a.cfg.Token, proto.Version)
		if errors.Is(err, tunnel.ErrUnauthorized) {
			return err
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			a.log.Warn("cannot reach server, retrying", "err", err, "in", backoff)
		} else {
			a.log.Info("connected to chipin", "server", a.cfg.Server, "resources", len(a.order))
			backoff = time.Second
			srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 30 * time.Second}
			go func() {
				select {
				case <-ctx.Done():
					sess.Close()
				case <-sess.CloseChan():
				}
			}()
			srv.Serve(sess) // returns once the session closes
			if ctx.Err() != nil {
				return nil
			}
			a.log.Warn("disconnected from chipin, reconnecting")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// Handler serves requests arriving over the tunnel.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.StatusPath, a.handleStatus)
	mux.HandleFunc("/r/{res}/v1/{path...}", a.handleResource)
	return mux
}

func (a *Agent) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := proto.Status{Version: proto.Version, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU()}
	for _, res := range a.order {
		st.Resources = append(st.Resources, res.status(r.Context()))
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *Agent) handleResource(w http.ResponseWriter, r *http.Request) {
	res, ok := a.resources[r.PathValue("res")]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown resource")
		return
	}
	apiPath := "/v1/" + r.PathValue("path")
	method, ok := proto.OpenAIPaths[apiPath]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "endpoint not shared")
		return
	}
	if r.Method != method {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	if r.Method == http.MethodPost {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", "request body too large")
			return
		}
		if res.allowed != nil {
			var peek struct {
				Model string `json:"model"`
			}
			json.Unmarshal(body, &peek)
			if !res.allowed[peek.Model] {
				writeError(w, http.StatusNotFound, "model_not_found", "model not shared")
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}

	if res.sem != nil {
		select {
		case res.sem <- struct{}{}:
			defer func() { <-res.sem }()
		default:
			writeError(w, http.StatusTooManyRequests, "busy", "resource is at capacity")
			return
		}
	}
	res.inflight.Add(1)
	defer res.inflight.Add(-1)

	r.URL.Path = apiPath
	res.proxy.ServeHTTP(w, r)
}

// rewrite points a tunnelled request at the upstream and makes sure nothing
// from the far side (consumer credentials, addresses, cookies) leaks through.
func (r *resource) rewrite(pr *httputil.ProxyRequest) {
	target := *r.upstream
	target.Path = r.upstream.Path + strings.TrimPrefix(pr.In.URL.Path, "/v1")
	target.RawQuery = ""
	pr.Out.URL = &target
	pr.Out.Host = ""
	h := pr.Out.Header
	for _, k := range []string{"Authorization", "Cookie", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		h.Del(k)
	}
	if r.APIKey != "" {
		h.Set("Authorization", "Bearer "+r.APIKey)
	}
}

func (r *resource) status(ctx context.Context) proto.ResourceStatus {
	st := proto.ResourceStatus{Name: r.Name, Kind: r.Kind, MaxConcurrent: r.MaxConcurrent, InFlight: r.inflight.Load(), Models: []string{}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.upstream.String()+"/models", nil)
	if r.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.APIKey)
	}
	start := time.Now()
	resp, err := r.client.Do(req)
	st.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		st.Error = "upstream unreachable: " + err.Error()
		return st
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		st.Error = fmt.Sprintf("upstream returned %s", resp.Status)
		return st
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		st.Error = "bad /models response: " + err.Error()
		return st
	}
	for _, m := range list.Data {
		if r.allowed == nil || r.allowed[m.ID] {
			st.Models = append(st.Models, m.ID)
		}
	}
	st.Healthy = true
	st.Loaded, st.LoadedKnown = r.loadedModels(ctx)
	return st
}

// loadedModels asks an Ollama upstream which models are in memory. Other
// upstreams (vLLM, hosted APIs) keep their models loaded, so "unknown" is
// treated as warm.
func (r *resource) loadedModels(ctx context.Context) ([]string, bool) {
	root := strings.TrimSuffix(r.upstream.String(), "/v1")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/ps", nil)
	if r.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.APIKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var ps struct {
		Models *[]struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ps) != nil || ps.Models == nil {
		return nil, false
	}
	loaded := []string{}
	for _, m := range *ps.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		if r.allowed == nil || r.allowed[name] {
			loaded = append(loaded, name)
		}
	}
	return loaded, true
}

// upstreamTransport keeps enough idle connections to the upstream for every
// concurrent request. Go's default keeps 2 per host, so under load each
// request would open a new TCP connection and the host runs out of ports.
func upstreamTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   512,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func isLocal(host string) bool {
	return host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"type": typ, "message": msg}})
}
