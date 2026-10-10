package chip

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUpstream records what reached the "local AI daemon".
type fakeUpstream struct {
	paths   []string
	headers []http.Header
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.headers = append(f.headers, r.Header.Clone())
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
