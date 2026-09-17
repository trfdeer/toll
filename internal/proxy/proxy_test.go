package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

func newTestLogger() *log.Logger {
	l := log.NewWithOptions(nil, log.Options{Level: log.ErrorLevel, ReportTimestamp: false})
	return l
}

func TestStreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("upstream Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []string{`data: {"a":1}`, "", `data: [DONE]`, ""} {
			_, _ = w.Write([]byte(chunk + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	base, _ := url.Parse(upstream.URL)
	h := New(base, "upstream-secret", newTestLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := "data: {\"a\":1}\n\n\n\ndata: [DONE]\n\n\n\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestPathMapping(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	base, _ := url.Parse(upstream.URL + "/v1")
	h := New(base, "k", newTestLogger())

	for _, tc := range []struct{ in, want string }{
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/v1/models", "/v1/models"},
		{"/v1/responses", "/v1/responses"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", tc.in, nil))
		if gotPath != tc.want {
			t.Errorf("path for %s = %q, want %q", tc.in, gotPath, tc.want)
		}
		if rec.Code != http.StatusTeapot {
			t.Errorf("status = %d, want 418 (errors must pass through verbatim)", rec.Code)
		}
	}
}

func TestUpstreamDown(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:1") // nothing listens here
	h := New(base, "k", newTestLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestImmediateFlush(t *testing.T) {
	ch := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		<-ch
	}))
	defer upstream.Close()
	defer close(ch)

	base, _ := url.Parse(upstream.URL)
	h := New(base, "k", newTestLogger())

	srv := httptest.NewServer(h)
	defer srv.Close()

	start := time.Now()
	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The first chunk must arrive well before the upstream handler unblocks.
	errCh := make(chan error, 1)
	go func() {
		n, err := io.ReadFull(resp.Body, make([]byte, len("data: first\n\n")))
		if n != len("data: first\n\n") {
			t.Errorf("read %d bytes", n)
		}
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("first chunk read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("first chunk did not arrive within 2s; streaming is buffered")
	}
	_ = start
}
