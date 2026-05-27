package sipua

import (
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"

	"github.com/stefandsl/bellerophon-go/internal/config"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

// TestBuildRegisterRequestIsSticky verifies that two successive REGISTERs
// reuse the same Call-ID and bump the CSeq monotonically — the property
// RFC 3261 §10.2 requires for the registrar to treat refreshes as updates
// of the same binding rather than new registrations.
func TestBuildRegisterRequestIsSticky(t *testing.T) {
	s, err := NewServer(config.SIP{
		Domain:        "pbx.example.com",
		Registrar:     "pbx.example.com",
		RegistrarPort: 5060,
		Extension:     "200",
		Expiry:        60,
	}, Options{
		LocalAddr: "10.0.0.5:5070",
		Logger:    bellog.New("error", "text"),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var recipient sip.Uri
	if err := sip.ParseUri("sip:200@pbx.example.com:5060", &recipient); err != nil {
		t.Fatalf("ParseUri: %v", err)
	}

	req1, cseq1 := s.buildRegisterRequest(recipient, 60)
	// Simulate sipgo's RegisterBuild bumping the CSeq before send, then
	// update our sticky counter from the wire value.
	if c := req1.CSeq(); c != nil {
		s.regCSeq = c.SeqNo
	}

	req2, cseq2 := s.buildRegisterRequest(recipient, 60)

	if got1, got2 := req1.CallID().Value(), req2.CallID().Value(); got1 != got2 {
		t.Errorf("Call-ID must be sticky across refreshes: %q vs %q", got1, got2)
	}
	if cseq2 <= cseq1 {
		t.Errorf("CSeq must be monotonically increasing: cseq1=%d cseq2=%d", cseq1, cseq2)
	}
	if c := req1.Contact(); c == nil {
		t.Error("Contact header missing on first REGISTER")
	}
	if got := s.regContact; !strings.Contains(got, "200") {
		t.Errorf("cached contact missing extension: %q", got)
	}
}

func TestCallTableSnapshotReturnsCopy(t *testing.T) {
	var ct callTable
	ct.put("a", &Call{CallID: "a"})
	ct.put("b", &Call{CallID: "b"})

	snap := ct.snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}

	// Mutating the table afterwards must not change the snapshot.
	ct.drop("a")
	if len(snap) != 2 {
		t.Errorf("snapshot mutated by table drop: len=%d", len(snap))
	}
	if got := ct.get("a"); got != nil {
		t.Errorf("get('a') after drop = %v, want nil", got)
	}
	if got := ct.get("b"); got == nil {
		t.Error("get('b') = nil, want present")
	}
}

func TestRandomCallIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := randomCallID()
		if seen[id] {
			t.Fatalf("randomCallID collision after %d iterations: %q", i, id)
		}
		seen[id] = true
		if !strings.Contains(id, "@bellerophon") && !strings.HasPrefix(id, "bellerophon-") {
			t.Errorf("randomCallID %q has unexpected shape", id)
		}
	}
}
