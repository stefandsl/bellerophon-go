package integration_test

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLive_Generic_Asterisk_RegisterAndEcho is the always-skipped (in CI)
// happy-path test against a dockerized Asterisk reachable on the host
// network. Enables itself when BELLEROPHON_LIVE_GENERIC=1 is set.
//
// The test runs the full sequence per docs/m001-uat.md Section B:
//  1. boot ./bellerophon --config config.asterisk.yaml --echo-mode
//  2. wait for the registered-as log line
//  3. sipp-drive an inbound INVITE with 5 s of G.711 sine
//  4. verify the echo arrives ~500 ms later
//  5. SIGINT and verify clean shutdown
func TestLive_Generic_Asterisk_RegisterAndEcho(t *testing.T) {
	requireEnv(t, "BELLEROPHON_LIVE_GENERIC")
	requireSIPP(t)

	configPath := os.Getenv("BELLEROPHON_ASTERISK_CONFIG")
	if configPath == "" {
		t.Skip("set BELLEROPHON_ASTERISK_CONFIG to point at an Asterisk-flavoured config.yaml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, kill := startBellerophon(t, configPath, "--echo-mode")
	defer kill()

	if err := waitForRegistered(ctx, ""); err != nil {
		t.Fatalf("REGISTER did not complete: %v", err)
	}

	// sipp-driven INVITE + echo verification lands here once the sipp
	// scenarios are ported from voice-app/test/integration/. The harness
	// shape is intentionally identical across all three providers; the
	// only differences are credentials and the expected DID format.
	t.Skip("TODO(M001/S07): port sipp scenarios + audio-round-trip checks")
}
