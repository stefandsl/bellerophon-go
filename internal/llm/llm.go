// Package llm defines the language-model client interface for the
// Bellerophon conversation loop and ships two concrete implementations:
//
//   - Anthropic — direct calls to api.anthropic.com /v1/messages. Default
//     when ANTHROPIC_API_KEY is set and CLAUDE_API_URL is empty.
//   - Bridge — HTTP fallback that POSTs to ${CLAUDE_API_URL}/ask, matching
//     the contract spoken by voice-app/lib/claude-bridge.js. Used when the
//     operator wants Claude Code CLI (or one of its alternative backends)
//     to mediate, instead of going to Anthropic directly.
//
// Both impls use a SessionStore (sessions.go) so multi-turn context
// survives binary restarts: Anthropic stores the full message history
// per call ID; Bridge stores the upstream sessionId string returned by
// claude-api-server (consumed via --resume on the next turn).
//
// The interface is intentionally narrow — Query and EndSession — so the
// conversation state machine in M002 S05 can swap backends without
// reaching into vendor-specific types.
package llm

import (
	"context"
	"fmt"
)

// Client is the abstract language-model interface every concrete backend
// implements. Implementations should be safe for concurrent calls from
// different callIDs; per-callID ordering is the caller's responsibility
// (the conversation loop serializes turns inside one call).
type Client interface {
	// Query sends one turn to the LLM and returns its reply. CallID
	// keys the session so multi-turn context resumes across requests
	// — pass the same CallID across the lifetime of a phone call.
	// An empty CallID is allowed (one-shot, no history kept).
	Query(ctx context.Context, req Request) (Response, error)

	// EndSession releases any local + upstream state held for callID.
	// Calling EndSession on an unknown callID is a no-op (not an error)
	// so the conversation loop can call it unconditionally on hangup.
	EndSession(ctx context.Context, callID string) error
}

// Request is one turn from the caller. The fields mirror the parameters
// voice-app's claude-bridge.js sends to /ask, so the two backends share
// a single calling shape.
type Request struct {
	// CallID is the conversation key. Empty = one-shot, no history.
	CallID string
	// Prompt is the user's text for this turn. Required.
	Prompt string
	// DevicePrompt is the per-device persona / system prompt. Optional;
	// empty means "use the model's default behaviour". Only applied on
	// the first turn of a session — subsequent turns reuse the system
	// message recorded in the SessionStore.
	DevicePrompt string
	// Backend is an optional per-call override for the Bridge backend
	// ("claude" / "codex" / "opencode" / "ollama" / "gemini"), passed
	// straight through to claude-api-server. Ignored by Anthropic.
	Backend string
}

// Response carries the model's reply plus the upstream session marker
// the next turn will use. SessionID is opaque to the caller — it is the
// implementation's choice (Anthropic returns its own conversation id;
// Bridge returns whatever claude-api-server stored under callId).
type Response struct {
	Text       string
	SessionID  string
	DurationMs int64
}

// HTTPError is returned when the upstream service answers with a non-2xx
// status. Callers — typically the conversation loop's retry layer —
// inspect StatusCode to decide between transient backoff (429, 5xx) and
// hard failure (4xx). Body holds the raw response (truncated for safety)
// so debug logs show the upstream error envelope verbatim.
type HTTPError struct {
	// Source identifies which client surfaced the error ("anthropic" or
	// "bridge") so a unified log line tells the operator where to look.
	Source     string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Source, e.StatusCode, truncate(e.Body, 200))
}

// truncate caps s at maxLen bytes, appending "..." when it had to clip.
// Anthropic and Cloud-front error pages can be many kilobytes — letting
// them flow unbounded into logs would obscure the actual signal.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
