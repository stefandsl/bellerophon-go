package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

// DefaultAnthropicBaseURL is Anthropic's production endpoint.
const DefaultAnthropicBaseURL = "https://api.anthropic.com"

// DefaultAnthropicModel is the cost/latency-balanced default for voice
// conversations as of 2026-05. Operators tune via AnthropicOptions.Model.
const DefaultAnthropicModel = "claude-haiku-4-5-20251001"

// DefaultAnthropicVersion is the API version header value. Required by
// Anthropic on every Messages request; pinned here so a server-side
// breaking change doesn't silently alter response shape.
const DefaultAnthropicVersion = "2023-06-01"

// DefaultAnthropicMaxTokens caps reply length. Voice replies are short
// (the TTS layer makes long ones jarring), so 1024 is plenty of head-room
// without inviting runaway generations.
const DefaultAnthropicMaxTokens = 1024

// DefaultAnthropicTimeout is the per-request wall-clock cap when the
// caller's context has no deadline. Conversation latency budget is
// ~2.5 s end-to-end (M002 success criterion), so 30 s here is a hard
// safety net rather than a normal-path number.
const DefaultAnthropicTimeout = 30 * time.Second

// AnthropicOptions configures NewAnthropic. APIKey is the only required
// field; everything else has a sensible default.
type AnthropicOptions struct {
	APIKey  string
	Model   string
	BaseURL string
	Version string
	// MaxTokens caps reply length. 0 → DefaultAnthropicMaxTokens.
	MaxTokens int
	// Client lets callers inject a *http.Client with custom timeouts /
	// transport. Nil → a fresh client with DefaultAnthropicTimeout.
	Client *http.Client
	// Store is the session journal (see sessions.go). Nil → fresh
	// in-memory store; histories then don't survive a restart.
	Store *SessionStore
	// Logger is optional; nil → silent.
	Logger bellog.Logger
}

// Anthropic is the hand-rolled Messages API client. It implements
// Client. State (conversation history) lives in Store, keyed by callID
// — Anthropic's HTTP API is stateless, so we keep the running message
// list on our side and resend it every turn.
type Anthropic struct {
	apiKey    string
	model     string
	baseURL   string
	version   string
	maxTokens int
	client    *http.Client
	store     *SessionStore
	logger    bellog.Logger
}

// Compile-time interface assertion. *Anthropic must satisfy Client or
// callers wiring up the conversation loop won't compile.
var _ Client = (*Anthropic)(nil)

// NewAnthropic builds the client. Returns an error if APIKey is empty
// — failing fast at construction beats a 401 on the first call.
func NewAnthropic(opts AnthropicOptions) (*Anthropic, error) {
	if opts.APIKey == "" {
		return nil, errors.New("llm: Anthropic APIKey is required")
	}
	model := opts.Model
	if model == "" {
		model = DefaultAnthropicModel
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultAnthropicBaseURL
	}
	base = strings.TrimRight(base, "/")
	version := opts.Version
	if version == "" {
		version = DefaultAnthropicVersion
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultAnthropicMaxTokens
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultAnthropicTimeout}
	}
	store := opts.Store
	if store == nil {
		store = NewSessionStore()
	}
	return &Anthropic{
		apiKey:    opts.APIKey,
		model:     model,
		baseURL:   base,
		version:   version,
		maxTokens: maxTokens,
		client:    client,
		store:     store,
		logger:    opts.Logger,
	}, nil
}

// anthropicMessage is one turn in the conversation. The Messages API
// alternates "user" and "assistant"; we never emit "system" here — the
// system prompt rides in a separate top-level field.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicRequest mirrors the JSON shape /v1/messages expects. We only
// populate System when the caller supplied a DevicePrompt and there is
// no history yet (first turn of the call).
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

// anthropicResponse captures the fields we consume from the success
// envelope. Extra fields (id, type, model, stop_reason, usage…) are
// ignored on purpose so a new server field can't fail the unmarshal.
type anthropicResponse struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// sessionState is what we serialize into SessionStore for one callID.
// Persisting System alongside Messages means the persona survives a
// restart — without it, the second-turn behaviour after a crash would
// silently lose its system prompt.
type sessionState struct {
	System   string             `json:"system,omitempty"`
	Messages []anthropicMessage `json:"messages"`
}

// Query sends one turn. On success it appends user+assistant to the
// session history and persists the updated state before returning, so
// a crash between turns can't lose the just-completed exchange.
func (a *Anthropic) Query(ctx context.Context, req Request) (Response, error) {
	if req.Prompt == "" {
		return Response{}, errors.New("llm: anthropic: empty prompt")
	}

	state := a.loadOrInit(req.CallID, req.DevicePrompt)

	// Append the user turn before sending so a stop-the-world failure
	// inside Do() doesn't lose what the caller said.
	state.Messages = append(state.Messages, anthropicMessage{
		Role:    "user",
		Content: req.Prompt,
	})

	body := anthropicRequest{
		Model:     a.model,
		MaxTokens: a.maxTokens,
		System:    state.System,
		Messages:  state.Messages,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: anthropic: marshal request: %w", err)
	}

	endpoint := a.baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("llm: anthropic: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", a.version)

	if a.logger != nil {
		a.logger.Debug("anthropic query",
			"endpoint", endpoint,
			"model", a.model,
			"call_id", req.CallID,
			"turns", len(state.Messages))
	}

	started := time.Now()
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("llm: anthropic: request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: anthropic: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Response{}, &HTTPError{
			Source:     "anthropic",
			StatusCode: resp.StatusCode,
			Body:       string(respBytes),
		}
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return Response{}, fmt.Errorf("llm: anthropic: parse response: %w (body=%q)",
			err, truncate(string(respBytes), 200))
	}
	text := extractText(parsed)
	if text == "" {
		return Response{}, fmt.Errorf("llm: anthropic: empty content in response (id=%s)", parsed.ID)
	}

	// Persist the assistant turn so the next Query resumes correctly.
	state.Messages = append(state.Messages, anthropicMessage{
		Role:    "assistant",
		Content: text,
	})
	if err := a.saveState(req.CallID, state); err != nil {
		// A persistence failure shouldn't fail the call — log it and
		// continue with the in-memory history. The caller already got
		// a successful reply; making the user re-ask because the disk
		// is full would be worse than silently degraded durability.
		if a.logger != nil {
			a.logger.Warn("anthropic session persist failed",
				"call_id", req.CallID, "err", err.Error())
		}
	}

	return Response{
		Text:       text,
		SessionID:  parsed.ID,
		DurationMs: time.Since(started).Milliseconds(),
	}, nil
}

// EndSession drops the conversation history for callID. Anthropic has
// no server-side state to release, so this is purely a local cleanup.
func (a *Anthropic) EndSession(_ context.Context, callID string) error {
	if callID == "" {
		return nil
	}
	return a.store.Delete(callID)
}

// loadOrInit replays the persisted history for callID, or returns a
// fresh state seeded with devicePrompt as the system message. devicePrompt
// is only honoured on first-turn — once a history exists the original
// system message is the source of truth (callers can't mid-call swap
// the persona without ending the session).
func (a *Anthropic) loadOrInit(callID, devicePrompt string) sessionState {
	if callID == "" {
		return sessionState{System: devicePrompt}
	}
	raw, ok := a.store.Get(callID)
	if !ok || raw == "" {
		return sessionState{System: devicePrompt}
	}
	var st sessionState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		// Corrupt journal entry — start fresh rather than crash. The
		// caller's persona is the best fallback we have.
		if a.logger != nil {
			a.logger.Warn("anthropic session corrupt, resetting",
				"call_id", callID, "err", err.Error())
		}
		return sessionState{System: devicePrompt}
	}
	return st
}

// saveState writes state back to the SessionStore as JSON. Caller-side
// errors (full disk, RO mount) bubble up so callers can log/metric them.
func (a *Anthropic) saveState(callID string, st sessionState) error {
	if callID == "" {
		return nil
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return a.store.Put(callID, string(raw))
}

// extractText concatenates every text block in the response. Anthropic
// streams content as a list of typed blocks (text / tool_use / …); we
// only care about "text" for the conversation loop in M002. Tool use
// lands in a later milestone.
func extractText(r anthropicResponse) string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
