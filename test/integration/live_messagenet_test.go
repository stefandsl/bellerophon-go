package integration_test

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLive_MessageNet_RegisterInboundDID gates on
// BELLEROPHON_LIVE_MESSAGENET=1. It is the dedicated DID-provider leg of
// the multi-provider M001 UAT — MessageNet is a SIP trunk (not a PBX)
// that sells Italian DIDs. The test exercises the sipprov.MessageNet
// quirks that don't show up on Asterisk or 3CX:
//
//   - DID arrives in the To: URI without country code; LocalDID.E164
//     should carry the +39 prefix.
//   - Authorization handling on the first REGISTER must work.
//   - OPTIONS keepalive at the 25 s MessageNet cadence keeps the binding
//     alive across a 90 s idle stretch.
func TestLive_MessageNet_RegisterInboundDID(t *testing.T) {
	requireEnv(t, "BELLEROPHON_LIVE_MESSAGENET")
	requireSIPP(t)

	configPath := os.Getenv("BELLEROPHON_MESSAGENET_CONFIG")
	if configPath == "" {
		t.Skip("set BELLEROPHON_MESSAGENET_CONFIG to point at a MessageNet config.yaml")
	}
	did := os.Getenv("MESSAGENET_DID")
	if did == "" {
		t.Skip("set MESSAGENET_DID to a number you can dial from a PSTN test source")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, kill := startBellerophon(t, configPath, "--echo-mode")
	defer kill()

	if err := waitForRegistered(ctx, ""); err != nil {
		t.Fatalf("REGISTER did not complete: %v", err)
	}

	// The MessageNet-specific assertions go here:
	//  - Sleep ≥ 90 s and verify the binding is still alive (no
	//    "binding stale" log line, no need to re-REGISTER mid-test
	//    other than the scheduled refresh).
	//  - Trigger an inbound test call via PSTN (manual today; could be
	//    automated against a soft-phone in a future cycle).
	//  - Inspect the subprocess log for the LocalDID line and confirm
	//    LocalDID.E164 begins with "+39" while LocalDID.Raw does not.
	t.Skip("TODO(M001/S07): inbound-call automation against MessageNet; manual today (see docs/m001-uat.md §A)")
}
