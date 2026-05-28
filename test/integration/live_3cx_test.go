package integration_test

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLive_3CX_ContactQuirksAndIdleSurvival gates on BELLEROPHON_LIVE_3CX=1.
// It is the dedicated 3CX leg of the multi-provider M001 UAT and exercises
// the sipprov.ThreeCX quirks that don't show up on the other two:
//
//   - REGISTER Contact MUST carry ;transport=udp and a stable
//     ;rinstance=<16hex>. Without them 3CX may register us successfully
//     then evict the binding on its next stale-registration sweep.
//   - 3CX disconnects extensions after ~10 min idle; the 5-min OPTIONS
//     cadence in sipprov.New3CX() must keep the binding alive across a
//     12-min sleep.
//   - DID is the extension number verbatim; LocalDID.E164 == LocalDID.Raw.
//
// Stefan's existing 3CX deployment is the test target.
func TestLive_3CX_ContactQuirksAndIdleSurvival(t *testing.T) {
	requireEnv(t, "BELLEROPHON_LIVE_3CX")
	requireSIPP(t)

	configPath := os.Getenv("BELLEROPHON_3CX_CONFIG")
	if configPath == "" {
		t.Skip("set BELLEROPHON_3CX_CONFIG to point at a 3CX config.yaml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, kill := startBellerophon(t, configPath, "--echo-mode")
	defer kill()

	if err := waitForRegistered(ctx, ""); err != nil {
		t.Fatalf("REGISTER did not complete: %v", err)
	}

	// 3CX-specific assertions go here:
	//   - tcpdump the REGISTER (or hook the sipgo layer if we expose it)
	//     and assert Contact carries `transport=udp` and `rinstance=`
	//     16 hex chars. Different from MessageNet (which has neither).
	//   - Run for ≥ 12 min and verify the binding survives. Today this is
	//     the slowest UAT step; skipping behind a separate gate
	//     BELLEROPHON_3CX_IDLE_SURVIVAL=1 would let CI run the fast
	//     contact-quirks check and reserve the slow leg for nightly.
	//   - sipp-driven extension-to-extension test call exercises the
	//     happy path.
	t.Skip("TODO(M001/S07): contact-header pcap assertions + 12-min idle survival check")
}
