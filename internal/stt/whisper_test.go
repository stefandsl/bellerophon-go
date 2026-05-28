package stt

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

// pcmTone produces ms milliseconds of a 1 kHz sine at 16 kHz mono PCM16.
// Used as a stand-in for a real speech utterance in the test harness.
func pcmTone(ms int) []int16 {
	const rate = 16000
	n := rate * ms / 1000
	out := make([]int16, n)
	for i := range out {
		// Constant amplitude is fine — Whisper sees the WAV envelope,
		// the contents don't matter for unit tests with a mock server.
		out[i] = 4000
	}
	return out
}

func TestNewWhisper_RejectsMissingAPIKey(t *testing.T) {
	t.Parallel()
	_, err := NewWhisper(WhisperOptions{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Errorf("expected APIKey-required error, got %v", err)
	}
}

func TestNewWhisper_FillsDefaults(t *testing.T) {
	t.Parallel()
	w, err := NewWhisper(WhisperOptions{APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("NewWhisper: %v", err)
	}
	if w.model != DefaultWhisperModel {
		t.Errorf("model = %q, want %q", w.model, DefaultWhisperModel)
	}
	if w.baseURL != DefaultWhisperBaseURL {
		t.Errorf("baseURL = %q, want %q", w.baseURL, DefaultWhisperBaseURL)
	}
	if w.client == nil || w.client.Timeout != DefaultWhisperTimeout {
		t.Errorf("client missing default timeout")
	}
}

func TestNewWhisper_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	t.Parallel()
	w, _ := NewWhisper(WhisperOptions{APIKey: "k", BaseURL: "https://x.example/v1/"})
	if w.baseURL != "https://x.example/v1" {
		t.Errorf("baseURL = %q, trailing slash not trimmed", w.baseURL)
	}
}

// TestTranscribe_HappyPath drives the full request shape through a mock
// server and verifies the parsed transcript comes back.
func TestTranscribe_HappyPath(t *testing.T) {
	t.Parallel()
	var (
		gotAuth, gotPath, gotContentType, gotModel, gotFormat, gotLang string
		gotFileLen                                                     int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")

		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		gotModel = r.FormValue("model")
		gotFormat = r.FormValue("response_format")
		gotLang = r.FormValue("language")

		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(rw, "no file: "+err.Error(), 400)
			return
		}
		defer f.Close()
		body, _ := io.ReadAll(f)
		gotFileLen = len(body)

		// Smoke check: the file looks like a WAV (starts with RIFF).
		if len(body) < 12 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
			http.Error(rw, "not a WAV", 400)
			return
		}

		_ = json.NewEncoder(rw).Encode(map[string]string{"text": "  ciao mondo  "})
	}))
	defer srv.Close()

	w, err := NewWhisper(WhisperOptions{
		APIKey:   "sk-test",
		BaseURL:  srv.URL,
		Language: "it",
	})
	if err != nil {
		t.Fatalf("NewWhisper: %v", err)
	}

	text, err := w.Transcribe(context.Background(), pcmTone(500), 16000)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "ciao mondo" {
		t.Errorf("text = %q, want %q (whitespace must be trimmed)", text, "ciao mondo")
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want 'Bearer sk-test'", gotAuth)
	}
	if gotPath != "/audio/transcriptions" {
		t.Errorf("path = %q, want '/audio/transcriptions'", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data; boundary=") {
		t.Errorf("Content-Type = %q, want multipart/form-data", gotContentType)
	}
	if gotModel != DefaultWhisperModel {
		t.Errorf("model = %q, want %q", gotModel, DefaultWhisperModel)
	}
	if gotFormat != "json" {
		t.Errorf("response_format = %q, want 'json'", gotFormat)
	}
	if gotLang != "it" {
		t.Errorf("language = %q, want 'it'", gotLang)
	}
	if gotFileLen == 0 {
		t.Error("file part was empty")
	}
}

// TestTranscribe_OmitsLanguageWhenUnset confirms we don't send an empty
// language field — empty would suppress Whisper's auto-detection.
func TestTranscribe_OmitsLanguageWhenUnset(t *testing.T) {
	t.Parallel()
	var langSeen string
	var langSet atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		// FormValue returns "" for both absent and explicitly-empty.
		// Inspect the raw multipart to disambiguate.
		if _, ok := r.MultipartForm.Value["language"]; ok {
			langSet.Store(true)
			langSeen = r.MultipartForm.Value["language"][0]
		}
		_ = json.NewEncoder(rw).Encode(map[string]string{"text": "ok"})
	}))
	defer srv.Close()

	w, _ := NewWhisper(WhisperOptions{APIKey: "k", BaseURL: srv.URL})
	if _, err := w.Transcribe(context.Background(), pcmTone(200), 16000); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if langSet.Load() {
		t.Errorf("language field sent unexpectedly with value %q", langSeen)
	}
}

func TestTranscribe_RejectsEmptySamples(t *testing.T) {
	t.Parallel()
	w, _ := NewWhisper(WhisperOptions{APIKey: "k"})
	if _, err := w.Transcribe(context.Background(), nil, 16000); err == nil {
		t.Error("nil samples: expected error")
	}
	if _, err := w.Transcribe(context.Background(), []int16{}, 16000); err == nil {
		t.Error("empty samples: expected error")
	}
}

func TestTranscribe_RejectsZeroSampleRate(t *testing.T) {
	t.Parallel()
	w, _ := NewWhisper(WhisperOptions{APIKey: "k"})
	if _, err := w.Transcribe(context.Background(), pcmTone(100), 0); err == nil {
		t.Error("rate=0: expected error")
	}
}

// TestTranscribe_HTTPErrorIsTyped pins down the HTTPError shape so the
// upstream retry logic can branch on StatusCode.
func TestTranscribe_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()
	for _, code := range []int{400, 401, 403, 413, 429, 500, 502, 503} {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(code)
				_, _ = rw.Write([]byte(`{"error":{"message":"bad"}}`))
			}))
			defer srv.Close()
			w, _ := NewWhisper(WhisperOptions{APIKey: "k", BaseURL: srv.URL})
			_, err := w.Transcribe(context.Background(), pcmTone(100), 16000)
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
		})
	}
}

func TestTranscribe_MalformedJSONErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	w, _ := NewWhisper(WhisperOptions{APIKey: "k", BaseURL: srv.URL})
	_, err := w.Transcribe(context.Background(), pcmTone(100), 16000)
	if err == nil || !strings.Contains(err.Error(), "parse response") {
		t.Errorf("expected parse-response error, got %v", err)
	}
}

func TestTranscribe_ContextCancelAbortsRequest(t *testing.T) {
	t.Parallel()
	// Server hangs for 2 s; client cancels after 50 ms.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	w, _ := NewWhisper(WhisperOptions{APIKey: "k", BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := w.Transcribe(ctx, pcmTone(100), 16000)
	if err == nil {
		t.Fatal("expected error on ctx cancel")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancel ignored: elapsed %v (request did not abort)", elapsed)
	}
}

// TestProviderInterfaceAssertion is a compile-time-equivalent runtime
// check: a *Whisper must satisfy the Provider interface.
func TestProviderInterfaceAssertion(t *testing.T) {
	t.Parallel()
	var _ Provider = (*Whisper)(nil)
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("under-limit: %q", got)
	}
	if got := truncate("0123456789abcdef", 5); got != "01234..." {
		t.Errorf("truncated: %q, want '01234...'", got)
	}
}
