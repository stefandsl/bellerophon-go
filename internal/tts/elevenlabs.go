package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

// DefaultElevenLabsBaseURL is ElevenLabs's production endpoint.
const DefaultElevenLabsBaseURL = "https://api.elevenlabs.io/v1"

// DefaultElevenLabsModel matches voice-app's default — fastest first-byte
// (~75 ms on streaming, ~400-600 ms on the REST endpoint) for voice
// conversations. Override via ElevenLabsOptions.Model when an operator
// wants eleven_v3 for audio tags or eleven_multilingual_v2 for prosody.
const DefaultElevenLabsModel = "eleven_flash_v2_5"

// DefaultElevenLabsVoiceID is the Bellerophon-branded voice from
// voice-app/lib/tts-service.js. Operators override per-call by passing
// voiceID to Synthesize; the constant only kicks in when both the
// call-site arg AND ElevenLabsOptions.DefaultVoiceID are empty.
const DefaultElevenLabsVoiceID = "JAgnJveGGUh4qy4kh6dF"

// DefaultElevenLabsTimeout caps a single render. Flash on a 200-char
// reply finishes in well under 1 s; 30 s is a hard safety net for
// upstream slowness rather than a normal-path number.
const DefaultElevenLabsTimeout = 30 * time.Second

// outputFormat is the ElevenLabs format token for raw mono 16-bit LE
// PCM at 16 kHz — what the rest of internal/ expects. Hard-coded so
// callers can't accidentally pick a format the codec layer can't
// resample (output_format=pcm_8000 would work for telephony but the
// codec layer already does 16↔8 downsampling).
const outputFormat = "pcm_16000"

// ElevenLabsOptions configures NewElevenLabs. APIKey is the only
// required field; everything else has a sensible default that mirrors
// voice-app/lib/tts-service.js so the JS and Go stacks render the
// same phrase to the same audio.
type ElevenLabsOptions struct {
	APIKey string
	// Model overrides the ELEVENLABS_MODEL_ID env value voice-app
	// uses. Empty → DefaultElevenLabsModel.
	Model string
	// BaseURL overrides the API origin. Test code uses
	// httptest.NewServer().URL here.
	BaseURL string
	// DefaultVoiceID is the fallback used when Synthesize is called
	// with an empty voiceID. Empty → DefaultElevenLabsVoiceID.
	DefaultVoiceID string
	// VoiceSettings overrides the default stability / similarity /
	// style / speaker-boost. Zero value means "use voice-app's
	// defaults" (0.5 / 0.75 / 0.0 / true) — empirically tuned for
	// conversational pace, so changing them is opt-in.
	VoiceSettings *VoiceSettings
	// Client lets callers inject a *http.Client with custom timeouts
	// / transport. Nil → fresh client with DefaultElevenLabsTimeout.
	Client *http.Client
	// Cache is the on-disk PCM cache. Nil → no caching (every call
	// hits the network — fine for tests, not for production).
	Cache *Cache
	// Logger is optional; nil → silent.
	Logger bellog.Logger
}

// VoiceSettings mirrors the voice_settings sub-object ElevenLabs
// expects in the request body. Values match voice-app's defaults.
type VoiceSettings struct {
	Stability       float64 `json:"stability"`
	SimilarityBoost float64 `json:"similarity_boost"`
	Style           float64 `json:"style"`
	UseSpeakerBoost bool    `json:"use_speaker_boost"`
}

// DefaultVoiceSettings returns voice-app's tuned defaults. Calling code
// uses these when no override is supplied; tests pin them to avoid
// drift between the JS and Go stacks.
func DefaultVoiceSettings() VoiceSettings {
	return VoiceSettings{
		Stability:       0.5,
		SimilarityBoost: 0.75,
		Style:           0.0,
		UseSpeakerBoost: true,
	}
}

// ElevenLabs is the hand-rolled HTTP client. It implements Provider.
// Multi-stack-parity is the explicit design goal: same request shape,
// same model defaults, same voice-not-found fallback behaviour as
// voice-app — so a phrase the Node stack already cached returns the
// same bytes (up to ElevenLabs determinism) under the Go stack.
type ElevenLabs struct {
	apiKey         string
	model          string
	baseURL        string
	defaultVoiceID string
	voiceSettings  VoiceSettings
	client         *http.Client
	cache          *Cache
	logger         bellog.Logger
}

// Compile-time interface assertion.
var _ Provider = (*ElevenLabs)(nil)

// NewElevenLabs builds the client. Returns an error if APIKey is empty
// — failing fast at construction beats a 401 on the first call.
func NewElevenLabs(opts ElevenLabsOptions) (*ElevenLabs, error) {
	if opts.APIKey == "" {
		return nil, errors.New("tts: elevenlabs APIKey is required")
	}
	model := opts.Model
	if model == "" {
		model = DefaultElevenLabsModel
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultElevenLabsBaseURL
	}
	base = strings.TrimRight(base, "/")
	defaultVoice := opts.DefaultVoiceID
	if defaultVoice == "" {
		defaultVoice = DefaultElevenLabsVoiceID
	}
	settings := DefaultVoiceSettings()
	if opts.VoiceSettings != nil {
		settings = *opts.VoiceSettings
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultElevenLabsTimeout}
	}
	return &ElevenLabs{
		apiKey:         opts.APIKey,
		model:          model,
		baseURL:        base,
		defaultVoiceID: defaultVoice,
		voiceSettings:  settings,
		client:         client,
		cache:          opts.Cache,
		logger:         opts.Logger,
	}, nil
}

// ttsRequest is the body shape ElevenLabs's /text-to-speech endpoint
// expects. Field names match voice-app/lib/tts-service.js verbatim so
// a future API change touches both stacks the same way.
type ttsRequest struct {
	Text          string        `json:"text"`
	ModelID       string        `json:"model_id"`
	VoiceSettings VoiceSettings `json:"voice_settings"`
}

// Synthesize renders text to PCM16/16k bytes via ElevenLabs. The cache
// (when configured) shortcuts the upstream call on a hit; on a miss the
// network request is wrapped in single-flight so concurrent callers
// for the same phrase share one synthesis.
//
// voiceID is the per-call override; empty falls through to
// DefaultVoiceID (constructor option) and then DefaultElevenLabsVoiceID.
// A voice_not_found 404 retries once with the default voice — matches
// voice-app's behaviour so a malformed device config can't kill the
// call.
func (e *ElevenLabs) Synthesize(ctx context.Context, text, voiceID string) ([]byte, error) {
	if text == "" {
		return nil, errors.New("tts: elevenlabs empty text")
	}
	effective := voiceID
	if effective == "" {
		effective = e.defaultVoiceID
	}

	if e.cache != nil {
		key := Key(text, effective, e.model)
		return e.cache.Generate(ctx, key, func(ctx context.Context) ([]byte, error) {
			return e.fetch(ctx, text, effective, true)
		})
	}
	return e.fetch(ctx, text, effective, true)
}

// fetch performs one render. tryFallback controls whether a 404
// voice_not_found retries with the default voice — only the outer call
// asks for the retry to avoid an infinite loop if the default voice is
// itself the broken one.
func (e *ElevenLabs) fetch(ctx context.Context, text, voiceID string, tryFallback bool) ([]byte, error) {
	body, err := json.Marshal(ttsRequest{
		Text:          text,
		ModelID:       e.model,
		VoiceSettings: e.voiceSettings,
	})
	if err != nil {
		return nil, fmt.Errorf("tts: elevenlabs marshal: %w", err)
	}

	endpoint := fmt.Sprintf("%s/text-to-speech/%s?output_format=%s",
		e.baseURL, url.PathEscape(voiceID), outputFormat)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tts: elevenlabs new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("xi-api-key", e.apiKey)
	// Accept is informational here — output_format wins server-side —
	// but voice-app sends it and ElevenLabs's docs recommend it, so we
	// match for parity. application/octet-stream because the response
	// is raw PCM, not MP3.
	httpReq.Header.Set("Accept", "application/octet-stream")

	if e.logger != nil {
		e.logger.Debug("elevenlabs synthesize",
			"endpoint", endpoint,
			"model", e.model,
			"voice", voiceID,
			"text_len", len(text))
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tts: elevenlabs request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		errBytes, _ := io.ReadAll(resp.Body)
		// voice_not_found is the one case where voice-app retries:
		// per-device voiceIDs can rot if a voice gets deleted in the
		// ElevenLabs library, and rather than failing the call we
		// switch to a known-good default. The check is body-text
		// rather than a structured field because ElevenLabs returns
		// {detail:{status:"voice_not_found",...}} and the canonical
		// signal is the string.
		if resp.StatusCode == http.StatusNotFound &&
			tryFallback &&
			voiceID != e.defaultVoiceID &&
			bytes.Contains(errBytes, []byte("voice_not_found")) {
			if e.logger != nil {
				e.logger.Warn("elevenlabs voice_not_found, falling back",
					"requested", voiceID, "fallback", e.defaultVoiceID)
			}
			return e.fetch(ctx, text, e.defaultVoiceID, false)
		}
		return nil, &HTTPError{
			Source:     "elevenlabs",
			StatusCode: resp.StatusCode,
			Body:       string(errBytes),
		}
	}

	pcm, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tts: elevenlabs read body: %w", err)
	}
	if len(pcm) == 0 {
		return nil, errors.New("tts: elevenlabs empty audio body")
	}
	return pcm, nil
}
