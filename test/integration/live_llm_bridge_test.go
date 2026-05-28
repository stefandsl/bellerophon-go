package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stefandsl/bellerophon-go/internal/llm"
)

// TestLive_LLM_Bridge exercises the claude-api-server HTTP fallback
// against a running instance of claude-api-server (the JS one in
// bellerophon/claude-api-server). Gated on BELLEROPHON_LIVE_BRIDGE=1.
//
// The server has to be reachable at CLAUDE_API_URL (default
// http://localhost:3333). The test posts /ask with a trivial prompt
// and checks for a non-empty reply, then calls /end-session.
//
// Pins:
//
//  1. The /ask body shape (prompt, callId, devicePrompt, backend) is
//     accepted by the JS server. A mis-shaped body would 400 here.
//  2. {success: false} envelopes surface as *llm.HTTPError so the
//     retry layer can branch on them. (Not exercised in the happy
//     path; covered by the unit test, but the live path is the canary
//     for an API contract drift.)
//  3. /end-session returns 2xx after a successful /ask round-trip.
//
// Env contract:
//
//	CLAUDE_API_URL  (optional; default http://localhost:3333)
//	BRIDGE_BACKEND  (optional override: claude / codex / opencode / ollama / gemini)
func TestLive_LLM_Bridge(t *testing.T) {
	requireEnv(t, "BELLEROPHON_LIVE_BRIDGE")
	baseURL := os.Getenv("CLAUDE_API_URL")
	if baseURL == "" {
		baseURL = "http://localhost:3333"
	}

	client, err := llm.NewBridge(llm.BridgeOptions{
		BaseURL: baseURL,
		Backend: os.Getenv("BRIDGE_BACKEND"),
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const callID = "live-bridge-test"
	defer func() { _ = client.EndSession(context.Background(), callID) }()

	r, err := client.Query(ctx, llm.Request{
		CallID:       callID,
		Prompt:       "Reply with exactly the word 'pong' and nothing else.",
		DevicePrompt: "You are a test bot. Always obey instructions literally.",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	t.Logf("bridge reply (server-reported %d ms, sessionID=%q): %q",
		r.DurationMs, r.SessionID, r.Text)
	if r.Text == "" {
		t.Fatal("bridge returned empty reply")
	}

	if err := client.EndSession(ctx, callID); err != nil {
		t.Errorf("EndSession: %v", err)
	}
}
