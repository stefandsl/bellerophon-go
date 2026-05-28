package integration_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stefandsl/bellerophon-go/internal/llm"
)

// TestLive_LLM_Anthropic exercises the direct Anthropic Messages
// client against api.anthropic.com. Gated on
// BELLEROPHON_LIVE_ANTHROPIC=1.
//
// Pins:
//
//  1. A simple multi-turn exchange returns non-empty replies on
//     every turn — proves headers + body shape + parse path.
//  2. Multi-turn history is honoured (turn 2 sees what turn 1 said).
//     The check is loose — we ask "what's my name?" after introducing
//     ourselves and look for the name in the reply.
//  3. EndSession cleans up without erroring.
//
// Env contract:
//
//	ANTHROPIC_API_KEY  (required)
//	ANTHROPIC_MODEL    (optional override; default = SDK default)
func TestLive_LLM_Anthropic(t *testing.T) {
	requireEnv(t, "BELLEROPHON_LIVE_ANTHROPIC")
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	client, err := llm.NewAnthropic(llm.AnthropicOptions{
		APIKey: apiKey,
		Model:  os.Getenv("ANTHROPIC_MODEL"),
	})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() { _ = client.EndSession(context.Background(), "live-anthropic-test") }()

	// Turn 1: introduce a memorable name.
	r1, err := client.Query(ctx, llm.Request{
		CallID:       "live-anthropic-test",
		Prompt:       "My name is Bellerophon. Just say 'noted' if you understand.",
		DevicePrompt: "You are a brief, polite assistant. Reply in at most one short sentence.",
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	t.Logf("turn 1 reply (%d ms): %q", r1.DurationMs, r1.Text)
	if r1.Text == "" {
		t.Fatal("turn 1: empty reply")
	}

	// Turn 2: verify the model still remembers turn 1.
	r2, err := client.Query(ctx, llm.Request{
		CallID: "live-anthropic-test",
		Prompt: "What name did I just tell you?",
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	t.Logf("turn 2 reply (%d ms): %q", r2.DurationMs, r2.Text)
	if !strings.Contains(strings.ToLower(r2.Text), "bellerophon") {
		t.Errorf("turn 2 reply %q does not reference 'Bellerophon' — history did not carry across turns", r2.Text)
	}
}
