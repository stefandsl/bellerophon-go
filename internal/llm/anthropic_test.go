package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockMessagesHandler returns a handler that responds with the given
// reply text on success and records the body it received. callCount
// tracks invocations so tests can assert per-call behaviour.
type mockMessagesHandler struct {
	callCount   atomic.Int32
	lastBody    atomic.Pointer[anthropicRequest]
	lastAuth    atomic.Pointer[string]
	lastVersion atomic.Pointer[string]
	replyText   string
	replyID     string
	statusCode  int
	rawBody     string // when non-empty, override JSON encoding
}

func (m *mockMessagesHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	m.callCount.Add(1)
	auth := r.Header.Get("x-api-key")
	m.lastAuth.Store(&auth)
	v := r.Header.Get("anthropic-version")
	m.lastVersion.Store(&v)

	body, _ := io.ReadAll(r.Body)
	var parsed anthropicRequest
	_ = json.Unmarshal(body, &parsed)
	m.lastBody.Store(&parsed)

	if r.URL.Path != "/v1/messages" {
		http.Error(rw, "wrong path: "+r.URL.Path, 404)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		http.Error(rw, "wrong content-type: "+ct, 400)
		return
	}

	status := m.statusCode
	if status == 0 {
		status = 200
	}
	rw.WriteHeader(status)
	if m.rawBody != "" {
		_, _ = rw.Write([]byte(m.rawBody))
		return
	}
	_ = json.NewEncoder(rw).Encode(anthropicResponse{
		ID:   pickNonEmpty(m.replyID, "msg_test"),
		Role: "assistant",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: m.replyText}},
	})
}

func pickNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestNewAnthropic_RejectsMissingAPIKey(t *testing.T) {
	t.Parallel()
	_, err := NewAnthropic(AnthropicOptions{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Errorf("expected APIKey-required error, got %v", err)
	}
}

func TestNewAnthropic_FillsDefaults(t *testing.T) {
	t.Parallel()
	a, err := NewAnthropic(AnthropicOptions{APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	if a.model != DefaultAnthropicModel {
		t.Errorf("model = %q, want %q", a.model, DefaultAnthropicModel)
	}
	if a.baseURL != DefaultAnthropicBaseURL {
		t.Errorf("baseURL = %q, want %q", a.baseURL, DefaultAnthropicBaseURL)
	}
	if a.version != DefaultAnthropicVersion {
		t.Errorf("version = %q, want %q", a.version, DefaultAnthropicVersion)
	}
	if a.maxTokens != DefaultAnthropicMaxTokens {
		t.Errorf("maxTokens = %d, want %d", a.maxTokens, DefaultAnthropicMaxTokens)
	}
	if a.store == nil {
		t.Error("expected default in-memory store")
	}
}

func TestNewAnthropic_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: "https://x.example/"})
	if a.baseURL != "https://x.example" {
		t.Errorf("baseURL = %q, trailing slash not trimmed", a.baseURL)
	}
}

func TestAnthropic_Query_HappyPath_SendsHeadersAndPersona(t *testing.T) {
	t.Parallel()
	mock := &mockMessagesHandler{replyText: "ciao mondo"}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	a, err := NewAnthropic(AnthropicOptions{
		APIKey:  "sk-test-123",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}

	resp, err := a.Query(context.Background(), Request{
		CallID:       "call-1",
		Prompt:       "salve",
		DevicePrompt: "you are Bellerophon",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.Text != "ciao mondo" {
		t.Errorf("text = %q, want ciao mondo", resp.Text)
	}
	if resp.SessionID != "msg_test" {
		t.Errorf("sessionID = %q, want msg_test", resp.SessionID)
	}

	if got := *mock.lastAuth.Load(); got != "sk-test-123" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := *mock.lastVersion.Load(); got != DefaultAnthropicVersion {
		t.Errorf("anthropic-version = %q", got)
	}
	body := *mock.lastBody.Load()
	if body.Model != DefaultAnthropicModel {
		t.Errorf("model in body = %q", body.Model)
	}
	if body.MaxTokens != DefaultAnthropicMaxTokens {
		t.Errorf("max_tokens = %d", body.MaxTokens)
	}
	if body.System != "you are Bellerophon" {
		t.Errorf("system = %q", body.System)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" ||
		body.Messages[0].Content != "salve" {
		t.Errorf("messages = %+v", body.Messages)
	}
}

// TestAnthropic_Query_MultiTurnHistoryPersists drives two turns and
// verifies the second request includes the first turn's history.
func TestAnthropic_Query_MultiTurnHistoryPersists(t *testing.T) {
	t.Parallel()
	var lastMessages atomic.Pointer[[]anthropicMessage]
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed anthropicRequest
		_ = json.Unmarshal(body, &parsed)
		msgs := parsed.Messages
		lastMessages.Store(&msgs)
		_ = json.NewEncoder(rw).Encode(anthropicResponse{
			ID:   "msg_x",
			Role: "assistant",
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "reply " + parsed.Messages[len(parsed.Messages)-1].Content}},
		})
	}))
	defer srv.Close()

	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: srv.URL})

	if _, err := a.Query(context.Background(), Request{
		CallID: "c1", Prompt: "first", DevicePrompt: "be brief",
	}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := a.Query(context.Background(), Request{
		CallID: "c1", Prompt: "second",
	}); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	msgs := *lastMessages.Load()
	if len(msgs) != 3 {
		t.Fatalf("turn 2 messages len = %d, want 3", len(msgs))
	}
	want := []anthropicMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply first"},
		{Role: "user", Content: "second"},
	}
	for i, m := range want {
		if msgs[i] != m {
			t.Errorf("msg[%d] = %+v, want %+v", i, msgs[i], m)
		}
	}
}

// TestAnthropic_Query_SessionResumeFromDisk writes a turn, builds a
// fresh client backed by the same store path, and confirms the second
// turn replays the history.
func TestAnthropic_Query_SessionResumeFromDisk(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.jsonl")

	var lastMessages atomic.Pointer[[]anthropicMessage]
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed anthropicRequest
		_ = json.Unmarshal(body, &parsed)
		msgs := parsed.Messages
		lastMessages.Store(&msgs)
		_ = json.NewEncoder(rw).Encode(anthropicResponse{
			ID: "id",
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "ack"}},
		})
	}))
	defer srv.Close()

	store, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	a1, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: srv.URL, Store: store})
	if _, err := a1.Query(context.Background(), Request{
		CallID: "c-resume", Prompt: "before crash", DevicePrompt: "persona",
	}); err != nil {
		t.Fatalf("pre-crash turn: %v", err)
	}
	_ = store.Close()

	store2, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	a2, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: srv.URL, Store: store2})
	if _, err := a2.Query(context.Background(), Request{
		CallID: "c-resume", Prompt: "after restart",
	}); err != nil {
		t.Fatalf("post-restart turn: %v", err)
	}

	msgs := *lastMessages.Load()
	if len(msgs) < 3 {
		t.Fatalf("post-restart messages len = %d, want >=3", len(msgs))
	}
	if msgs[0].Content != "before crash" {
		t.Errorf("history not replayed; msg[0] = %+v", msgs[0])
	}
}

func TestAnthropic_Query_RejectsEmptyPrompt(t *testing.T) {
	t.Parallel()
	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k"})
	if _, err := a.Query(context.Background(), Request{Prompt: ""}); err == nil {
		t.Error("empty prompt: expected error")
	}
}

func TestAnthropic_Query_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()
	for _, code := range []int{400, 401, 403, 413, 429, 500, 502, 503} {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(code)
				_, _ = rw.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
			}))
			defer srv.Close()
			a, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: srv.URL})
			_, err := a.Query(context.Background(), Request{Prompt: "hi"})
			if err == nil {
				t.Fatal("expected error")
			}
			var herr *HTTPError
			if !errors.As(err, &herr) {
				t.Fatalf("error is not *HTTPError: %v", err)
			}
			if herr.StatusCode != code {
				t.Errorf("StatusCode = %d, want %d", herr.StatusCode, code)
			}
			if herr.Source != "anthropic" {
				t.Errorf("Source = %q, want anthropic", herr.Source)
			}
		})
	}
}

func TestAnthropic_Query_MalformedJSONErrors(t *testing.T) {
	t.Parallel()
	mock := &mockMessagesHandler{rawBody: `{not json`}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: srv.URL})
	_, err := a.Query(context.Background(), Request{Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "parse response") {
		t.Errorf("expected parse-response error, got %v", err)
	}
}

func TestAnthropic_Query_EmptyContentErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte(`{"id":"x","role":"assistant","content":[]}`))
	}))
	defer srv.Close()

	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: srv.URL})
	_, err := a.Query(context.Background(), Request{Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "empty content") {
		t.Errorf("expected empty-content error, got %v", err)
	}
}

func TestAnthropic_Query_ContextCancelAbortsRequest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := a.Query(ctx, Request{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error on ctx cancel")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancel ignored: elapsed %v", elapsed)
	}
}

func TestAnthropic_EndSession_ClearsStore(t *testing.T) {
	t.Parallel()
	store := NewSessionStore()
	_ = store.Put("c1", `{"system":"","messages":[]}`)
	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k", Store: store})
	if err := a.EndSession(context.Background(), "c1"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if _, ok := store.Get("c1"); ok {
		t.Error("session still present after EndSession")
	}
}

// TestAnthropic_PersonaIgnoredOnSubsequentTurns documents the chosen
// behaviour: the DevicePrompt from turn 2+ is ignored when a history
// already exists. Mid-call persona swap is out of scope for M002.
func TestAnthropic_PersonaIgnoredOnSubsequentTurns(t *testing.T) {
	t.Parallel()
	var lastSystem atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed anthropicRequest
		_ = json.Unmarshal(body, &parsed)
		s := parsed.System
		lastSystem.Store(&s)
		_ = json.NewEncoder(rw).Encode(anthropicResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "ok"}},
		})
	}))
	defer srv.Close()

	a, _ := NewAnthropic(AnthropicOptions{APIKey: "k", BaseURL: srv.URL})
	_, _ = a.Query(context.Background(), Request{CallID: "c", Prompt: "1", DevicePrompt: "first persona"})
	_, _ = a.Query(context.Background(), Request{CallID: "c", Prompt: "2", DevicePrompt: "different persona"})

	if got := *lastSystem.Load(); got != "first persona" {
		t.Errorf("system on turn 2 = %q, want 'first persona' (mid-call swap not supported)", got)
	}
}
