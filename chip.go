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
	log      *slog.Logger
	lastErr  atomic.Pointer[string] // last status error logged, to log only changes
}

func New(cfg *Config, log *slog.Logger) (*Agent, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, resources: map[string]*resource{}, log: log}
	for _, rc := range cfg.Resources {
		u, _ := url.Parse(strings.TrimRight(rc.Upstream, "/"))
		transport := upstreamTransport()
		r := &resource{Resource: rc, upstream: u, client: &http.Client{Timeout: 5 * time.Second, Transport: transport}, log: log}
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
			ModifyResponse: func(resp *http.Response) error {
				resp.Header = keepHeaders(resp.Header, responseHeaders)
				return nil
			},
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

	if apiPath == "/v1/models" && res.allowed != nil {
		res.serveModels(w, r)
		return
	}

	// Take a slot before reading the body, so the hub can't make the chip
	// buffer a body for every stream it opens.
	if res.sem != nil {
		select {
		case res.sem <- struct{}{}:
			defer func() { <-res.sem }()
		default:
			writeError(w, http.StatusTooManyRequests, "busy", "resource is at capacity")
			return
		}
	}

	if r.Method == http.MethodPost {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", "request body too large")
			return
		}
		body, model, err := canonicalBody(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if res.allowed != nil && !res.allowed[model] {
			writeError(w, http.StatusNotFound, "model_not_found", "model not shared")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}

	res.inflight.Add(1)
	defer res.inflight.Add(-1)

	r.URL.Path = apiPath
	res.proxy.ServeHTTP(w, r)
}

// serveModels answers GET /v1/models with only the models the owner shares.
func (r *resource) serveModels(w http.ResponseWriter, req *http.Request) {
	up, _ := http.NewRequestWithContext(req.Context(), http.MethodGet, r.upstream.String()+"/models", nil)
	if r.APIKey != "" {
		up.Header.Set("Authorization", "Bearer "+r.APIKey)
	}
	resp, err := r.client.Do(up)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "the shared resource did not respond")
		return
	}
	defer resp.Body.Close()
	var list struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "the shared resource returned a bad model list")
		return
	}
	shared := []map[string]json.RawMessage{}
	for _, m := range list.Data {
		var id string
		if json.Unmarshal(m["id"], &id) == nil && r.allowed[id] {
			shared = append(shared, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": shared})
}

// canonicalBody parses a request body and re-encodes it, so the upstream gets
// exactly the JSON the chip checked. Go's decoder matches keys
// case-insensitively and keeps the last duplicate, while model servers read
// the exact key "model". Re-encoding drops duplicate keys, and any other
// spelling of "model" is refused.
func canonicalBody(body []byte) ([]byte, string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, "", errors.New("request body must be a JSON object")
	}
	for k := range m {
		if k != "model" && strings.EqualFold(k, "model") {
			return nil, "", fmt.Errorf("unexpected key %q", k)
		}
	}
	var model string
	if raw, ok := m["model"]; ok {
		if err := json.Unmarshal(raw, &model); err != nil {
			return nil, "", errors.New("model must be a string")
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, "", err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), model, nil
}

// rewrite points a tunnelled request at the upstream and forwards only the
// allowlisted headers, so nothing from the far side (consumer credentials,
// addresses, cookies) reaches it.
func (r *resource) rewrite(pr *httputil.ProxyRequest) {
	target := *r.upstream
	target.Path = r.upstream.Path + strings.TrimPrefix(pr.In.URL.Path, "/v1")
	target.RawQuery = ""
	pr.Out.URL = &target
	pr.Out.Host = ""
	pr.Out.Header = keepHeaders(pr.In.Header, requestHeaders)
	if r.APIKey != "" {
		pr.Out.Header.Set("Authorization", "Bearer "+r.APIKey)
	}
}

// Only these headers cross the chip. Everything else, including credentials,
// client addresses and server details, is dropped in both directions.
var (
	requestHeaders  = []string{"Content-Type", "Accept"}
	responseHeaders = []string{"Content-Type", "Content-Length", "Cache-Control"}
)

func keepHeaders(h http.Header, keep []string) http.Header {
	out := http.Header{}
	for _, k := range keep {
		if v := h.Values(k); len(v) > 0 {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}

// status reports the resource's health to the hub. Errors sent to the hub are
// short and generic. The full error, which can include local addresses and
// ports, is only logged here, once each time it changes.
func (r *resource) status(ctx context.Context) proto.ResourceStatus {
	st := r.checkStatus(ctx)
	last := ""
	if p := r.lastErr.Load(); p != nil {
		last = *p
	}
	if st.detail != last {
		r.lastErr.Store(&st.detail)
		if st.detail != "" {
			r.log.Warn("upstream unhealthy", "resource", r.Name, "err", st.detail)
		} else if last != "" {
			r.log.Info("upstream healthy again", "resource", r.Name)
		}
	}
	return st.ResourceStatus
}

type checkedStatus struct {
	proto.ResourceStatus
	detail string // full error for the local log
}

func (r *resource) checkStatus(ctx context.Context) (out checkedStatus) {
	st := &out.ResourceStatus
	*st = proto.ResourceStatus{Name: r.Name, Kind: r.Kind, MaxConcurrent: r.MaxConcurrent, InFlight: r.inflight.Load(), Models: []string{}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.upstream.String()+"/models", nil)
	if r.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.APIKey)
	}
	start := time.Now()
	resp, err := r.client.Do(req)
	st.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		st.Error, out.detail = "upstream unreachable", err.Error()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		st.Error = fmt.Sprintf("upstream returned %d", resp.StatusCode)
		out.detail = st.Error
		return
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		st.Error, out.detail = "upstream returned a bad model list", err.Error()
		return
	}
	for _, m := range list.Data {
		if r.allowed == nil || r.allowed[m.ID] {
			st.Models = append(st.Models, m.ID)
		}
	}
	st.Healthy = true
	st.Loaded, st.LoadedKnown = r.loadedModels(ctx)
	return
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
		ResponseHeaderTimeout: upstreamHeaderTimeout,
	}
}

// upstreamHeaderTimeout frees a slot held by a model server that never
// answers. It's long because a non-streamed response sends no headers until
// generation finishes, and loading a large model can take minutes.
var upstreamHeaderTimeout = 10 * time.Minute

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
