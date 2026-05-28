package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewBridge_RejectsMissingBaseURL(t *testing.T) {
	t.Parallel()
	_, err := NewBridge(BridgeOptions{})
	if err == nil || !strings.Contains(err.Error(), "BaseURL is required") {
		t.Errorf("expected BaseURL-required error, got %v", err)
	}
}

func TestNewBridge_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	b, _ := NewBridge(BridgeOptions{BaseURL: "http://localhost:3333/"})
	if b.baseURL != "http://localhost:3333" {
		t.Errorf("baseURL = %q, trailing slash not trimmed", b.baseURL)
	}
}

func TestBridge_Query_HappyPath(t *testing.T) {
	t.Parallel()
	var gotPath, gotContentType string
	var gotBody atomic.Pointer[askRequest]

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		var parsed askRequest
		_ = json.Unmarshal(raw, &parsed)
		gotBody.Store(&parsed)

		_ = json.NewEncoder(rw).Encode(askResponse{
			Success:    true,
			Response:   "the answer",
			SessionID:  "sess-42",
			DurationMs: 1234,
		})
	}))
	defer srv.Close()

	b, err := NewBridge(BridgeOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}

	resp, err := b.Query(context.Background(), Request{
		CallID:       "uuid-1",
		Prompt:       "ping",
		DevicePrompt: "be terse",
		Backend:      "claude",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.Text != "the answer" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.SessionID != "sess-42" {
		t.Errorf("sessionID = %q", resp.SessionID)
	}
	if resp.DurationMs != 1234 {
		t.Errorf("duration = %d, want 1234 (server-reported)", resp.DurationMs)
	}
	if gotPath != "/ask" {
		t.Errorf("path = %q, want /ask", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
	body := *gotBody.Load()
	if body.Prompt != "ping" || body.CallID != "uuid-1" ||
		body.DevicePrompt != "be terse" || body.Backend != "claude" {
		t.Errorf("body = %+v", body)
	}

	// SessionID should be stored under callID now.
	if got, ok := b.store.Get("uuid-1"); !ok || got != "sess-42" {
		t.Errorf("store[uuid-1] = (%q, %v), want (sess-42, true)", got, ok)
	}
}

// TestBridge_Query_DefaultBackendFallback documents that BridgeOptions.Backend
// is used when Request.Backend is empty.
func TestBridge_Query_DefaultBackendFallback(t *testing.T) {
	t.Parallel()
	var gotBackend atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed askRequest
		_ = json.Unmarshal(raw, &parsed)
		bk := parsed.Backend
		gotBackend.Store(&bk)
		_ = json.NewEncoder(rw).Encode(askResponse{Success: true, Response: "ok"})
	}))
	defer srv.Close()

	b, _ := NewBridge(BridgeOptions{BaseURL: srv.URL, Backend: "codex"})
	if _, err := b.Query(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := *gotBackend.Load(); got != "codex" {
		t.Errorf("backend = %q, want codex", got)
	}
}

func TestBridge_Query_RejectsEmptyPrompt(t *testing.T) {
	t.Parallel()
	b, _ := NewBridge(BridgeOptions{BaseURL: "http://x"})
	if _, err := b.Query(context.Background(), Request{Prompt: ""}); err == nil {
		t.Error("empty prompt: expected error")
	}
}

// TestBridge_Query_SuccessFalseSurfacesAsHTTPError pins the chosen
// behaviour for the server's "200 OK but failure" envelope.
func TestBridge_Query_SuccessFalseSurfacesAsHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(askResponse{
			Success: false,
			Error:   "claude cli exited 1",
		})
	}))
	defer srv.Close()

	b, _ := NewBridge(BridgeOptions{BaseURL: srv.URL})
	_, err := b.Query(context.Background(), Request{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if herr.Source != "bridge" {
		t.Errorf("source = %q", herr.Source)
	}
	if herr.Body != "claude cli exited 1" {
		t.Errorf("body = %q", herr.Body)
	}
}

func TestBridge_Query_NonSuccessStatusIsTyped(t *testing.T) {
	t.Parallel()
	for _, code := range []int{400, 404, 500, 502} {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(code)
				_, _ = rw.Write([]byte(`{"success":false,"error":"nope"}`))
			}))
			defer srv.Close()
			b, _ := NewBridge(BridgeOptions{BaseURL: srv.URL})
			_, err := b.Query(context.Background(), Request{Prompt: "hi"})
			var herr *HTTPError
			if !errors.As(err, &herr) {
				t.Fatalf("error is not *HTTPError: %v", err)
			}
			if herr.StatusCode != code {
				t.Errorf("StatusCode = %d, want %d", herr.StatusCode, code)
			}
		})
	}
}

func TestBridge_Query_MalformedJSONErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	b, _ := NewBridge(BridgeOptions{BaseURL: srv.URL})
	_, err := b.Query(context.Background(), Request{Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "parse response") {
		t.Errorf("expected parse-response error, got %v", err)
	}
}

func TestBridge_Query_ContextCancelAborts(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	b, _ := NewBridge(BridgeOptions{BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := b.Query(ctx, Request{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected ctx-cancel error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancel ignored, elapsed %v", elapsed)
	}
}

func TestBridge_Query_FallsBackToLocalDurationWhenServerOmits(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// duration_ms = 0 by omission
		_ = json.NewEncoder(rw).Encode(askResponse{Success: true, Response: "ok"})
	}))
	defer srv.Close()
	b, _ := NewBridge(BridgeOptions{BaseURL: srv.URL})
	resp, err := b.Query(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.DurationMs < 0 {
		t.Errorf("duration should be non-negative, got %d", resp.DurationMs)
	}
}

func TestBridge_EndSession_PostsAndClearsStore(t *testing.T) {
	t.Parallel()
	var gotPath atomic.Pointer[string]
	var gotBody atomic.Pointer[endSessionRequest]
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		gotPath.Store(&p)
		raw, _ := io.ReadAll(r.Body)
		var parsed endSessionRequest
		_ = json.Unmarshal(raw, &parsed)
		gotBody.Store(&parsed)
		rw.WriteHeader(200)
	}))
	defer srv.Close()

	b, _ := NewBridge(BridgeOptions{BaseURL: srv.URL})
	_ = b.store.Put("call-z", "sess-z")
	if err := b.EndSession(context.Background(), "call-z"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if got := *gotPath.Load(); got != "/end-session" {
		t.Errorf("path = %q", got)
	}
	if got := *gotBody.Load(); got.CallID != "call-z" {
		t.Errorf("body = %+v", got)
	}
	if _, ok := b.store.Get("call-z"); ok {
		t.Error("local entry not cleared")
	}
}

// TestBridge_EndSession_SwallowsServerErrors confirms a stuck or
// erroring server can't block call teardown.
func TestBridge_EndSession_SwallowsServerErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(500)
	}))
	defer srv.Close()
	b, _ := NewBridge(BridgeOptions{BaseURL: srv.URL})
	if err := b.EndSession(context.Background(), "any"); err != nil {
		t.Errorf("EndSession should swallow server 500, got %v", err)
	}
}

func TestBridge_EndSession_EmptyCallIDIsNoop(t *testing.T) {
	t.Parallel()
	b, _ := NewBridge(BridgeOptions{BaseURL: "http://not-called"})
	if err := b.EndSession(context.Background(), ""); err != nil {
		t.Errorf("empty callID should be noop, got %v", err)
	}
}
