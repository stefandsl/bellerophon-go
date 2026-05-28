package tts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pcmBlob returns a deterministic placeholder PCM payload. ElevenLabs's
// real output is opaque to the test — we only need to verify the bytes
// the server sent are the bytes the client returned.
func pcmBlob(seed byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed + byte(i&0x0f)
	}
	return out
}

func TestNewElevenLabs_RejectsMissingAPIKey(t *testing.T) {
	t.Parallel()
	_, err := NewElevenLabs(ElevenLabsOptions{})
	if err == nil || !strings.Contains(err.Error(), "APIKey is required") {
		t.Errorf("expected APIKey-required error, got %v", err)
	}
}

func TestNewElevenLabs_FillsDefaults(t *testing.T) {
	t.Parallel()
	e, err := NewElevenLabs(ElevenLabsOptions{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewElevenLabs: %v", err)
	}
	if e.model != DefaultElevenLabsModel {
		t.Errorf("model = %q, want %q", e.model, DefaultElevenLabsModel)
	}
	if e.baseURL != DefaultElevenLabsBaseURL {
		t.Errorf("baseURL = %q", e.baseURL)
	}
	if e.defaultVoiceID != DefaultElevenLabsVoiceID {
		t.Errorf("defaultVoiceID = %q", e.defaultVoiceID)
	}
	want := DefaultVoiceSettings()
	if e.voiceSettings != want {
		t.Errorf("voiceSettings = %+v, want %+v", e.voiceSettings, want)
	}
}

func TestNewElevenLabs_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	e, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: "https://x.example/v1/"})
	if e.baseURL != "https://x.example/v1" {
		t.Errorf("baseURL = %q, trailing slash not trimmed", e.baseURL)
	}
}

// TestSynthesize_HappyPath drives the full request shape through a mock
// server and verifies the PCM bytes come back unchanged.
func TestSynthesize_HappyPath(t *testing.T) {
	t.Parallel()
	want := pcmBlob(0xA0, 4096)
	var (
		gotAPIKey, gotAccept, gotCT, gotPath, gotFormat string
		gotBody                                         ttsRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("xi-api-key")
		gotAccept = r.Header.Get("Accept")
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		gotFormat = r.URL.Query().Get("output_format")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = rw.Write(want)
	}))
	defer srv.Close()

	e, err := NewElevenLabs(ElevenLabsOptions{APIKey: "sk-tts", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewElevenLabs: %v", err)
	}
	got, err := e.Synthesize(context.Background(), "ciao", "voice-A")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("returned bytes differ from server response")
	}
	if gotAPIKey != "sk-tts" {
		t.Errorf("xi-api-key = %q", gotAPIKey)
	}
	if gotAccept != "application/octet-stream" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotPath != "/text-to-speech/voice-A" {
		t.Errorf("path = %q", gotPath)
	}
	if gotFormat != outputFormat {
		t.Errorf("output_format = %q, want %q", gotFormat, outputFormat)
	}
	if gotBody.Text != "ciao" || gotBody.ModelID != DefaultElevenLabsModel {
		t.Errorf("body = %+v", gotBody)
	}
	if gotBody.VoiceSettings != DefaultVoiceSettings() {
		t.Errorf("voice_settings = %+v", gotBody.VoiceSettings)
	}
}

func TestSynthesize_EmptyVoiceFallsBackToDefault(t *testing.T) {
	t.Parallel()
	var gotPath atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		gotPath.Store(&p)
		_, _ = rw.Write(pcmBlob(1, 32))
	}))
	defer srv.Close()

	e, _ := NewElevenLabs(ElevenLabsOptions{
		APIKey:         "k",
		BaseURL:        srv.URL,
		DefaultVoiceID: "voice-default",
	})
	if _, err := e.Synthesize(context.Background(), "hi", ""); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if got := *gotPath.Load(); got != "/text-to-speech/voice-default" {
		t.Errorf("path = %q, want voice-default", got)
	}
}

func TestSynthesize_RejectsEmptyText(t *testing.T) {
	t.Parallel()
	e, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k"})
	if _, err := e.Synthesize(context.Background(), "", "v"); err == nil {
		t.Error("empty text: expected error")
	}
}

// TestSynthesize_VoiceNotFoundFallback: requested voice 404s with
// voice_not_found body → retry with default voice succeeds.
func TestSynthesize_VoiceNotFoundFallback(t *testing.T) {
	t.Parallel()
	var (
		callCount atomic.Int32
		paths     []string
		pathsMu   sync.Mutex
	)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		n := callCount.Add(1)
		if n == 1 {
			rw.WriteHeader(http.StatusNotFound)
			_, _ = rw.Write([]byte(`{"detail":{"status":"voice_not_found","message":"no"}}`))
			return
		}
		_, _ = rw.Write(pcmBlob(2, 64))
	}))
	defer srv.Close()

	e, _ := NewElevenLabs(ElevenLabsOptions{
		APIKey:         "k",
		BaseURL:        srv.URL,
		DefaultVoiceID: "good-voice",
	})
	pcm, err := e.Synthesize(context.Background(), "hi", "bad-voice")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(pcm) == 0 {
		t.Error("expected pcm bytes after fallback")
	}
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("expected 2 calls, got %d (%v)", len(paths), paths)
	}
	if paths[0] != "/text-to-speech/bad-voice" {
		t.Errorf("first call path = %q", paths[0])
	}
	if paths[1] != "/text-to-speech/good-voice" {
		t.Errorf("fallback path = %q", paths[1])
	}
}

// TestSynthesize_VoiceNotFoundOnDefaultDoesNotLoop guards against an
// infinite retry when the default voice is the broken one.
func TestSynthesize_VoiceNotFoundOnDefaultDoesNotLoop(t *testing.T) {
	t.Parallel()
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"detail":{"status":"voice_not_found"}}`))
	}))
	defer srv.Close()

	e, _ := NewElevenLabs(ElevenLabsOptions{
		APIKey: "k", BaseURL: srv.URL, DefaultVoiceID: "broken",
	})
	_, err := e.Synthesize(context.Background(), "hi", "broken")
	if err == nil {
		t.Fatal("expected HTTPError, got nil")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) || herr.StatusCode != 404 {
		t.Errorf("expected 404 HTTPError, got %v", err)
	}
	if c := callCount.Load(); c != 1 {
		t.Errorf("call count = %d, want 1 (no retry loop)", c)
	}
}

func TestSynthesize_HTTPErrorIsTyped(t *testing.T) {
	t.Parallel()
	for _, code := range []int{400, 401, 422, 429, 500, 502, 503} {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(code)
				_, _ = rw.Write([]byte(`{"detail":{"status":"server_error"}}`))
			}))
			defer srv.Close()
			e, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: srv.URL})
			_, err := e.Synthesize(context.Background(), "hi", "v")
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
			if herr.Source != "elevenlabs" {
				t.Errorf("Source = %q", herr.Source)
			}
		})
	}
}

func TestSynthesize_EmptyBodyErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// 200 OK with empty body — the server upstream sometimes does
		// this on quota issues, and silently returning zero-length PCM
		// would feed silence into the call.
	}))
	defer srv.Close()
	e, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: srv.URL})
	_, err := e.Synthesize(context.Background(), "hi", "v")
	if err == nil || !strings.Contains(err.Error(), "empty audio body") {
		t.Errorf("expected empty-body error, got %v", err)
	}
}

func TestSynthesize_ContextCancelAborts(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	e, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := e.Synthesize(ctx, "hi", "v")
	if err == nil {
		t.Fatal("expected ctx-cancel error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancel ignored, elapsed %v", elapsed)
	}
}

// TestSynthesize_CacheRoundTrip: first call hits the server, second
// call for the same (text, voice) returns cached bytes without
// hitting the network.
func TestSynthesize_CacheRoundTrip(t *testing.T) {
	t.Parallel()
	var callCount atomic.Int32
	body := pcmBlob(0x77, 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		_, _ = rw.Write(body)
	}))
	defer srv.Close()

	cache, err := NewCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	e, _ := NewElevenLabs(ElevenLabsOptions{
		APIKey:  "k",
		BaseURL: srv.URL,
		Cache:   cache,
	})

	a, err := e.Synthesize(context.Background(), "same text", "v1")
	if err != nil {
		t.Fatalf("synth a: %v", err)
	}
	b, err := e.Synthesize(context.Background(), "same text", "v1")
	if err != nil {
		t.Fatalf("synth b: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("cache returned different bytes")
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (second call should be a cache hit)", got)
	}
}

// TestSynthesize_CachePersistsAcrossInstances writes via one
// ElevenLabs client, then asks a fresh one with the same cache dir to
// resolve the same key — should hit disk, not the network.
func TestSynthesize_CachePersistsAcrossInstances(t *testing.T) {
	t.Parallel()
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		_, _ = rw.Write(pcmBlob(0xC0, 256))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cache1, _ := NewCache(dir, 0)
	e1, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: srv.URL, Cache: cache1})
	if _, err := e1.Synthesize(context.Background(), "persist me", "v"); err != nil {
		t.Fatalf("first synth: %v", err)
	}

	// Fresh cache instance, same dir — second client should read from disk.
	cache2, _ := NewCache(dir, 0)
	e2, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: srv.URL, Cache: cache2})
	if _, err := e2.Synthesize(context.Background(), "persist me", "v"); err != nil {
		t.Fatalf("second synth: %v", err)
	}
	if got := callCount.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (disk should have served the second)", got)
	}
}

// TestSynthesize_NetworkFailureNotCached verifies a render error does
// not poison the cache — the next call retries against the upstream.
func TestSynthesize_NetworkFailureNotCached(t *testing.T) {
	t.Parallel()
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			rw.WriteHeader(500)
			_, _ = rw.Write([]byte("transient"))
			return
		}
		_, _ = rw.Write(pcmBlob(0xEE, 16))
	}))
	defer srv.Close()

	cache, _ := NewCache(t.TempDir(), 0)
	e, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: srv.URL, Cache: cache})

	if _, err := e.Synthesize(context.Background(), "x", "v"); err == nil {
		t.Fatal("first call should fail")
	}
	if _, err := e.Synthesize(context.Background(), "x", "v"); err != nil {
		t.Fatalf("second call should retry past the failure, got %v", err)
	}
	if got := callCount.Load(); got != 2 {
		t.Errorf("server hits = %d, want 2", got)
	}
}

// TestSynthesize_SingleFlightCoalescing fires N concurrent goroutines
// on the same (text, voice) and verifies exactly one upstream call
// happens — the rest share the leader's result.
func TestSynthesize_SingleFlightCoalescing(t *testing.T) {
	t.Parallel()
	var (
		callCount atomic.Int32
		release   = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		<-release // hold the leader so the followers definitely arrive while it's still in-flight
		_, _ = rw.Write(pcmBlob(0x55, 128))
	}))
	defer srv.Close()

	cache, _ := NewCache(t.TempDir(), 0)
	e, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: srv.URL, Cache: cache})

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	results := make([][]byte, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			out, err := e.Synthesize(context.Background(), "shared", "v")
			results[i] = out
			errs[i] = err
		}()
	}
	// Let the goroutines pile up on the inflight slot, then release.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := callCount.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (single-flight didn't coalesce)", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
		if i > 0 && string(results[i]) != string(results[0]) {
			t.Errorf("goroutine %d got different bytes from goroutine 0", i)
		}
	}
}

func TestSynthesize_VoiceIDIsURLEscaped(t *testing.T) {
	t.Parallel()
	var gotPath atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		gotPath.Store(&p)
		_, _ = rw.Write(pcmBlob(0x10, 8))
	}))
	defer srv.Close()
	e, _ := NewElevenLabs(ElevenLabsOptions{APIKey: "k", BaseURL: srv.URL})
	if _, err := e.Synthesize(context.Background(), "hi", "weird voice/id"); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	got := *gotPath.Load()
	// The server-side URL.Path is already decoded; what matters is
	// that the request URL didn't blow up the routing. We assert the
	// decoded path equals what we sent.
	if got != "/text-to-speech/weird voice/id" {
		t.Errorf("decoded path = %q", got)
	}
}

func TestCustomVoiceSettings(t *testing.T) {
	t.Parallel()
	var gotBody ttsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = rw.Write(pcmBlob(0x01, 8))
	}))
	defer srv.Close()
	custom := VoiceSettings{Stability: 0.9, SimilarityBoost: 0.1, Style: 0.5, UseSpeakerBoost: false}
	e, _ := NewElevenLabs(ElevenLabsOptions{
		APIKey: "k", BaseURL: srv.URL, VoiceSettings: &custom,
	})
	_, _ = e.Synthesize(context.Background(), "hi", "v")
	if gotBody.VoiceSettings != custom {
		t.Errorf("voice_settings = %+v, want %+v", gotBody.VoiceSettings, custom)
	}
}

// TestProviderInterfaceAssertion is a runtime equivalent of the
// compile-time assertion in elevenlabs.go.
func TestProviderInterfaceAssertion(t *testing.T) {
	t.Parallel()
	var _ Provider = (*ElevenLabs)(nil)
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("short", 10); got != "short" {
		t.Errorf("under-limit: %q", got)
	}
	if got := truncate("0123456789abcdef", 5); got != "01234..." {
		t.Errorf("clipped: %q, want '01234...'", got)
	}
}

// Path-traversal regression: a malicious cache key should not be able
// to escape the cache dir. Key is hex-encoded md5, so this is mostly
// belt-and-suspenders — Put rejects an empty key and otherwise treats
// the input as a single file basename.
func TestCachePath_DoesNotEscapeDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, _ := NewCache(dir, 0)
	path := c.Path("../../etc/passwd")
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(dir)) {
		t.Errorf("path %q escaped dir %q", path, dir)
	}
}
