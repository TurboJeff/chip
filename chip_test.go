// Copyright 2026 Jeff Webb
// SPDX-License-Identifier: Apache-2.0

package chip

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeUpstream records what reached the "local AI daemon".
type fakeUpstream struct {
	paths   []string
	headers []http.Header
	bodies  []string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.headers = append(f.headers, r.Header.Clone())
	b, _ := io.ReadAll(r.Body)
	f.bodies = append(f.bodies, string(b))
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/models") {
		io.WriteString(w, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
		return
	}
	io.WriteString(w, `{"ok":true}`)
}

func newTestAgent(t *testing.T, res Resource) (*Agent, *fakeUpstream) {
	t.Helper()
	up := &fakeUpstream{}
	ts := httptest.NewServer(up)
	t.Cleanup(ts.Close)
	res.Name = "ai"
	res.Upstream = ts.URL + "/v1"
	a, err := New(&Config{Server: "http://x", Token: "t", Resources: []Resource{res}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return a, up
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestOnlyAllowlistedPathsReachUpstream(t *testing.T) {
	a, up := newTestAgent(t, Resource{})
	h := a.Handler()

	blocked := []struct{ method, path string }{
		{"POST", "/r/ai/v1/../api/pull"},
		{"POST", "/r/ai/api/pull"},
		{"DELETE", "/r/ai/api/delete"},
		{"POST", "/r/ai/v1/files"},
		{"GET", "/r/ai/v1/chat/completions"}, // wrong method
		{"POST", "/r/nope/v1/chat/completions"},
	}
	for _, b := range blocked {
		if rec := do(h, b.method, b.path, `{}`, nil); rec.Code == http.StatusOK {
			t.Errorf("%s %s: got %d, want it refused", b.method, b.path, rec.Code)
		}
	}
	if len(up.paths) != 0 {
		t.Fatalf("blocked requests reached upstream: %v", up.paths)
	}

	if rec := do(h, "POST", "/r/ai/v1/chat/completions", `{"model":"m1"}`, nil); rec.Code != 200 {
		t.Fatalf("allowed request: got %d %s", rec.Code, rec.Body)
	}
	if got := up.paths[0]; got != "POST /v1/chat/completions" {
		t.Fatalf("upstream got %q", got)
	}
}

func TestCredentialsAreReplacedNotForwarded(t *testing.T) {
	a, up := newTestAgent(t, Resource{APIKey: "donor-secret"})
	do(a.Handler(), "POST", "/r/ai/v1/chat/completions", `{"model":"m1"}`, map[string]string{
		"Authorization":   "Bearer consumer-key",
		"Cookie":          "x=1",
		"X-Forwarded-For": "1.2.3.4",
	})
	h := up.headers[0]
	if got := h.Get("Authorization"); got != "Bearer donor-secret" {
		t.Errorf("Authorization = %q", got)
	}
	for _, k := range []string{"Cookie", "X-Forwarded-For"} {
		if h.Get(k) != "" {
			t.Errorf("%s leaked to upstream: %q", k, h.Get(k))
		}
	}
}

func TestModelAllowlist(t *testing.T) {
	a, up := newTestAgent(t, Resource{Models: []string{"m1"}})
	h := a.Handler()
	if rec := do(h, "POST", "/r/ai/v1/chat/completions", `{"model":"m2"}`, nil); rec.Code != 404 {
		t.Errorf("unshared model: got %d", rec.Code)
	}
	if len(up.paths) != 0 {
		t.Fatalf("unshared model reached upstream")
	}

	rec := do(h, "GET", "/_chip/status", "", nil)
	var st struct {
		Resources []struct {
			Healthy bool
			Models  []string
		}
	}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if len(st.Resources) != 1 || !st.Resources[0].Healthy || len(st.Resources[0].Models) != 1 || st.Resources[0].Models[0] != "m1" {
		t.Fatalf("status = %s", rec.Body)
	}
}

// Go's decoder matches "MODEL" to the model field, but model servers read the
// exact key. A body that spells model two ways must not pass the allowlist as
// one model and run as another (issue #1).
func TestModelAllowlistCannotBeBypassed(t *testing.T) {
	a, up := newTestAgent(t, Resource{Models: []string{"m1"}})
	h := a.Handler()
	for _, body := range []string{
		`{"model":"m2","MODEL":"m1"}`,
		`{"model":"m2","Model":"m1"}`,
		`{"MODEL":"m1"}`,
		`{"model":["m1"]}`,
		`["m1"]`,
		`null`,
	} {
		if rec := do(h, "POST", "/r/ai/v1/chat/completions", body, nil); rec.Code == 200 {
			t.Errorf("%s: got 200, want it refused", body)
		}
	}
	if len(up.paths) != 0 {
		t.Fatalf("refused bodies reached upstream: %v", up.bodies)
	}

	// Exact duplicates collapse to the value the chip checked (the last one),
	// so the upstream can't pick the other.
	if rec := do(h, "POST", "/r/ai/v1/chat/completions", `{"model":"m2","model":"m1","prompt":"<a&b>"}`, nil); rec.Code != 200 {
		t.Fatalf("duplicate model keys: got %d %s", rec.Code, rec.Body)
	}
	if got, want := up.bodies[0], `{"model":"m1","prompt":"<a&b>"}`; got != want {
		t.Fatalf("upstream body = %s, want %s", got, want)
	}
}

// readTracker records whether anything read the request body.
type readTracker struct{ read atomic.Bool }

func (r *readTracker) Read(p []byte) (int, error) {
	r.read.Store(true)
	return 0, io.EOF
}

// A full resource answers 429 without reading the body, so a hub can't make
// the chip buffer a body for every stream it opens (issue #2).
func TestFullResourceDoesNotReadBody(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) })
	a, err := New(&Config{Server: "http://x", Token: "t", Resources: []Resource{
		{Name: "ai", Upstream: ts.URL + "/v1", MaxConcurrent: 1},
	}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	h := a.Handler()

	go do(h, "POST", "/r/ai/v1/chat/completions", `{"model":"m1"}`, nil)
	<-started

	body := &readTracker{}
	req := httptest.NewRequest("POST", "/r/ai/v1/chat/completions", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", rec.Code)
	}
	if body.read.Load() {
		t.Fatal("body was read while the resource was full")
	}
}
