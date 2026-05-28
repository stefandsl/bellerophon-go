package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
	"github.com/stefandsl/bellerophon-go/internal/media"
)

// DefaultWhisperBaseURL is OpenAI's production endpoint.
const DefaultWhisperBaseURL = "https://api.openai.com/v1"

// DefaultWhisperModel is the model name passed in the multipart form.
// `whisper-1` is the only generally-available production option as of
// 2026-05; OpenAI gates newer models behind preview flags.
const DefaultWhisperModel = "whisper-1"

// DefaultWhisperTimeout is the per-request wall-clock cap when the
// caller's context has no deadline. Transcription of a 10 s utterance
// completes well inside 5 s on OpenAI; 30 s is generous head-room.
const DefaultWhisperTimeout = 30 * time.Second

// HTTPError is returned when OpenAI responds with a non-2xx status.
// Callers (the conversation loop's retry layer) inspect StatusCode to
// decide between transient backoff (429, 5xx) and hard failure (4xx).
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("whisper: HTTP %d: %s", e.StatusCode, truncate(e.Body, 200))
}

// WhisperOptions configures NewWhisper.
type WhisperOptions struct {
	// APIKey is the OpenAI bearer token. Required.
	APIKey string
	// Model is the Whisper model name. Empty → DefaultWhisperModel.
	Model string
	// BaseURL overrides the OpenAI endpoint. Empty → DefaultWhisperBaseURL.
	// Test code uses httptest.NewServer().URL here.
	BaseURL string
	// Language is an optional ISO-639-1 hint ("it", "en", …). Omit for
	// auto-detection. Voice-app's default omits it.
	Language string
	// Client lets callers inject a *http.Client with custom timeouts /
	// transport. Nil → a fresh client with DefaultWhisperTimeout.
	Client *http.Client
	// Logger is optional; nil → silent.
	Logger bellog.Logger
}

// Whisper is the OpenAI Whisper HTTP client. Implements Provider.
type Whisper struct {
	apiKey   string
	model    string
	baseURL  string
	language string
	client   *http.Client
	logger   bellog.Logger
}

// Compile-time interface assertion.
var _ Provider = (*Whisper)(nil)

// NewWhisper builds a client. Returns an error if APIKey is empty.
func NewWhisper(opts WhisperOptions) (*Whisper, error) {
	if opts.APIKey == "" {
		return nil, errors.New("whisper: APIKey is required")
	}
	model := opts.Model
	if model == "" {
		model = DefaultWhisperModel
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultWhisperBaseURL
	}
	base = strings.TrimRight(base, "/")
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultWhisperTimeout}
	}
	return &Whisper{
		apiKey:   opts.APIKey,
		model:    model,
		baseURL:  base,
		language: opts.Language,
		client:   client,
		logger:   opts.Logger,
	}, nil
}

// Transcribe wraps the PCM16 samples in a WAV envelope and posts them
// to /audio/transcriptions as multipart form-data. Returns the
// "text" field from the JSON response, trimmed.
func (w *Whisper) Transcribe(ctx context.Context, samples []int16, sampleRate int) (string, error) {
	if len(samples) == 0 {
		return "", errors.New("whisper: empty sample buffer")
	}
	if sampleRate <= 0 {
		return "", fmt.Errorf("whisper: invalid sample rate %d", sampleRate)
	}

	var wav bytes.Buffer
	if err := media.WriteWAVPCM16Mono(&wav, samples, sampleRate); err != nil {
		return "", fmt.Errorf("whisper: wrap PCM as WAV: %w", err)
	}

	body, contentType, err := buildMultipart(wav.Bytes(), w.model, w.language)
	if err != nil {
		return "", fmt.Errorf("whisper: build multipart: %w", err)
	}

	endpoint := w.baseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("whisper: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.apiKey)
	req.Header.Set("Content-Type", contentType)

	if w.logger != nil {
		w.logger.Debug("whisper transcribe",
			"endpoint", endpoint,
			"sample_rate", sampleRate,
			"samples", len(samples),
			"wav_bytes", len(body))
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper: request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("whisper: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", &HTTPError{StatusCode: resp.StatusCode, Body: string(respBytes)}
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("whisper: parse response: %w (body=%q)", err, truncate(string(respBytes), 200))
	}
	return strings.TrimSpace(parsed.Text), nil
}

// buildMultipart serializes the WAV body + model + optional language
// into a multipart/form-data body. The file part filename is "audio.wav"
// and content-type "audio/wav" — Whisper sniffs the format from the
// magic bytes anyway, but supplying both matches what curl/SDK do.
func buildMultipart(wav []byte, model, language string) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// model
	if err := mw.WriteField("model", model); err != nil {
		return nil, "", err
	}
	// response_format=json so we always parse the same shape
	if err := mw.WriteField("response_format", "json"); err != nil {
		return nil, "", err
	}
	// optional language hint
	if language != "" {
		if err := mw.WriteField("language", language); err != nil {
			return nil, "", err
		}
	}

	// file part — Whisper allows wav/mp3/m4a/ogg/...; we send WAV.
	filePart, err := mw.CreatePart(textproto(
		`Content-Disposition`, `form-data; name="file"; filename="audio.wav"`,
		`Content-Type`, `audio/wav`,
	))
	if err != nil {
		return nil, "", err
	}
	if _, err := filePart.Write(wav); err != nil {
		return nil, "", err
	}

	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// textproto builds a small MIME header from alternating key/value pairs.
// Wrapper around net/textproto.MIMEHeader so the call site stays compact.
func textproto(kv ...string) map[string][]string {
	h := map[string][]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		h[kv[i]] = []string{kv[i+1]}
	}
	return h
}

// truncate clips s to maxLen runes, appending "..." if it was clipped.
// Used to keep error messages bounded when OpenAI returns a big HTML
// error page (e.g. 502 from upstream gateways).
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
