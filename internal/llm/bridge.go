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

// DefaultBridgeTimeout matches voice-app/lib/claude-bridge.js's
// default — claude-api-server can be slow when the Claude CLI re-spawns
// or the upstream backend (codex/gemini/...) is cold, so 30 s is the
// observed safe upper bound.
const DefaultBridgeTimeout = 30 * time.Second

// DefaultBridgeEndSessionTimeout caps the /end-session cleanup so a
// stuck server can't block call teardown. Matches the JS bridge.
const DefaultBridgeEndSessionTimeout = 5 * time.Second

// BridgeOptions configures NewBridge. BaseURL is the only required
// field; everything else has a sensible default. APIKey is intentionally
// absent — claude-api-server lives on the trusted internal network and
// authenticates upstream itself.
type BridgeOptions struct {
	// BaseURL is the claude-api-server origin, e.g. "http://localhost:3333".
	// Required. The /ask, /end-session and /health paths are appended.
	BaseURL string
	// Backend is an optional default override
	// ("claude" / "codex" / "opencode" / "ollama" / "gemini"). Request-
	// level Backend takes precedence; empty here means "let the server
	// pick" (matches today's voice-app behaviour).
	Backend string
	// Client lets callers inject a *http.Client with custom timeouts /
	// transport. Nil → a fresh client with DefaultBridgeTimeout.
	Client *http.Client
	// Store is the session journal. Nil → in-memory only; sessionIds
	// then don't survive a binary restart.
	Store *SessionStore
	// Logger is optional; nil → silent.
	Logger bellog.Logger
}

// Bridge is the HTTP client for claude-api-server. It implements Client.
// Unlike Anthropic, the server is the one that owns multi-turn context
// — we just remember the sessionId it returned so we can pass it back
// (claude-api-server uses callId, not sessionId, for resume; the
// SessionStore here keeps that mapping durable across restarts so the
// server's --resume logic continues to work).
type Bridge struct {
	baseURL string
	backend string
	client  *http.Client
	store   *SessionStore
	logger  bellog.Logger
}

// Compile-time interface assertion.
var _ Client = (*Bridge)(nil)

// NewBridge builds the client. Returns an error when BaseURL is empty
// (a typo'd CLAUDE_API_URL is the most common deployment foot-gun and
// failing fast at construction makes it obvious in the startup logs).
func NewBridge(opts BridgeOptions) (*Bridge, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("llm: bridge BaseURL is required (set CLAUDE_API_URL)")
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultBridgeTimeout}
	}
	store := opts.Store
	if store == nil {
		store = NewSessionStore()
	}
	return &Bridge{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		backend: opts.Backend,
		client:  client,
		store:   store,
		logger:  opts.Logger,
	}, nil
}

// askRequest matches the body claude-api-server expects on POST /ask.
// Field names are intentionally identical to claude-bridge.js so any
// future server-side change keeps both clients aligned.
type askRequest struct {
	Prompt       string `json:"prompt"`
	CallID       string `json:"callId,omitempty"`
	DevicePrompt string `json:"devicePrompt,omitempty"`
	Backend      string `json:"backend,omitempty"`
}

// askResponse matches the server's success envelope. Error envelopes
// reuse the same shape (Success=false, Error=...), so we decode in two
// passes: first check Success, then either parse Response or surface
// Error.
type askResponse struct {
	Success    bool   `json:"success"`
	Response   string `json:"response"`
	SessionID  string `json:"sessionId"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error"`
}

type endSessionRequest struct {
	CallID string `json:"callId"`
}

// Query POSTs the turn to /ask. On success we cache the returned
// sessionId — useful for debugging and for the planned future use case
// where bellerophon talks to the server out-of-band (e.g. to mint
// summaries) without going through the conversation loop.
func (b *Bridge) Query(ctx context.Context, req Request) (Response, error) {
	if req.Prompt == "" {
		return Response{}, errors.New("llm: bridge: empty prompt")
	}

	backend := req.Backend
	if backend == "" {
		backend = b.backend
	}

	body, err := json.Marshal(askRequest{
		Prompt:       req.Prompt,
		CallID:       req.CallID,
		DevicePrompt: req.DevicePrompt,
		Backend:      backend,
	})
	if err != nil {
		return Response{}, fmt.Errorf("llm: bridge: marshal request: %w", err)
	}

	endpoint := b.baseURL + "/ask"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("llm: bridge: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	if b.logger != nil {
		b.logger.Debug("bridge query",
			"endpoint", endpoint,
			"call_id", req.CallID,
			"backend", backend)
	}

	started := time.Now()
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("llm: bridge: request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: bridge: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Response{}, &HTTPError{
			Source:     "bridge",
			StatusCode: resp.StatusCode,
			Body:       string(respBytes),
		}
	}

	var parsed askResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return Response{}, fmt.Errorf("llm: bridge: parse response: %w (body=%q)",
			err, truncate(string(respBytes), 200))
	}
	if !parsed.Success {
		// Server-side application error: surface as HTTPError 200 so
		// callers handle it like any other upstream failure and don't
		// have to invent a second error type for "200 OK but failed".
		return Response{}, &HTTPError{
			Source:     "bridge",
			StatusCode: resp.StatusCode,
			Body:       parsed.Error,
		}
	}

	if req.CallID != "" && parsed.SessionID != "" {
		if err := b.store.Put(req.CallID, parsed.SessionID); err != nil {
			if b.logger != nil {
				b.logger.Warn("bridge session persist failed",
					"call_id", req.CallID, "err", err.Error())
			}
		}
	}

	duration := parsed.DurationMs
	if duration == 0 {
		duration = time.Since(started).Milliseconds()
	}
	return Response{
		Text:       parsed.Response,
		SessionID:  parsed.SessionID,
		DurationMs: duration,
	}, nil
}

// EndSession POSTs to /end-session and clears the local mapping. A
// failure on the server side is logged but not returned — call teardown
// must not be blocked by a sluggish bridge.
func (b *Bridge) EndSession(ctx context.Context, callID string) error {
	if callID == "" {
		return nil
	}
	if err := b.store.Delete(callID); err != nil && b.logger != nil {
		b.logger.Warn("bridge session delete (local) failed",
			"call_id", callID, "err", err.Error())
	}

	body, err := json.Marshal(endSessionRequest{CallID: callID})
	if err != nil {
		return fmt.Errorf("llm: bridge: marshal end-session: %w", err)
	}

	// Cap the upstream cleanup so a stuck server can't extend hang-up
	// latency. The local Delete above already succeeded — if the server
	// stays alive but holds the session, the worst case is a stale
	// entry that times out on its side.
	cleanupCtx, cancel := context.WithTimeout(ctx, DefaultBridgeEndSessionTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(cleanupCtx, http.MethodPost,
		b.baseURL+"/end-session", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("llm: bridge: new end-session request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		if b.logger != nil {
			b.logger.Warn("bridge end-session request failed",
				"call_id", callID, "err", err.Error())
		}
		return nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 && b.logger != nil {
		b.logger.Warn("bridge end-session non-2xx",
			"call_id", callID, "status", resp.StatusCode)
	}
	return nil
}
