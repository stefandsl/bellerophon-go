package sipprov

import (
	"testing"
	"time"
)

func TestGet_KnownKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		k    Kind
		want Kind
	}{
		{KindGeneric, KindGeneric},
		{KindMessageNet, KindMessageNet},
		{Kind3CX, Kind3CX},
	}
	for _, c := range cases {
		p, err := Get(c.k)
		if err != nil {
			t.Errorf("Get(%q): %v", c.k, err)
			continue
		}
		if p.Kind() != c.want {
			t.Errorf("Get(%q).Kind() = %q, want %q", c.k, p.Kind(), c.want)
		}
	}
}

func TestGet_UnknownKindErrors(t *testing.T) {
	t.Parallel()
	if _, err := Get("asterisk-pjsip"); err == nil {
		t.Error("Get(unknown): want error")
	}
	if _, err := Get(""); err == nil {
		t.Error("Get(empty): want error")
	}
}

func TestGeneric_PassthroughBehavior(t *testing.T) {
	t.Parallel()
	g := NewGeneric()
	cases := []string{"100", "+390123456789", "0123456789", "anything"}
	for _, s := range cases {
		did := g.NormalizeInboundDID(s)
		if did.E164 != s || did.Raw != s {
			t.Errorf("generic NormalizeInboundDID(%q) = %+v, want both fields %q", s, did, s)
		}
	}
	if g.RegisterHeaders() != nil {
		t.Error("generic RegisterHeaders should be nil")
	}
	if g.OptionsKeepaliveInterval() != 30*time.Second {
		t.Errorf("generic keepalive = %v, want 30s", g.OptionsKeepaliveInterval())
	}
	c := &ContactHints{User: "u", Host: "h", Port: 5060}
	g.RewriteContactForRegister(c)
	if len(c.Params) != 0 {
		t.Errorf("generic RewriteContact should be no-op; got %v", c.Params)
	}
	// nil-safe
	g.RewriteContactForRegister(nil)
}

func TestMessageNet_NormalizesItalianDIDs(t *testing.T) {
	t.Parallel()
	m := NewMessageNet()
	cases := []struct {
		in       string
		wantE164 string
	}{
		{"0123456789", "+39123456789"},     // leading 0 stripped
		{"3331234567", "+393331234567"},    // no leading 0
		{"+390123456789", "+390123456789"}, // already E.164 — leave alone
		{"100", "+39100"},                  // extension-like; still prefixed (caller decides if that's meaningful)
		{"", ""},                           // empty in, empty out
		{"not-a-number", "not-a-number"},   // non-digit input passes through
	}
	for _, c := range cases {
		got := m.NormalizeInboundDID(c.in)
		if got.E164 != c.wantE164 {
			t.Errorf("NormalizeInboundDID(%q).E164 = %q, want %q", c.in, got.E164, c.wantE164)
		}
		if got.Raw != c.in {
			t.Errorf("NormalizeInboundDID(%q).Raw = %q, want %q (must preserve)", c.in, got.Raw, c.in)
		}
	}
}

func TestMessageNet_CountryCodeOverride(t *testing.T) {
	t.Parallel()
	m := NewMessageNet()
	m.CountryCode = "+1" // hypothetical US deployment
	got := m.NormalizeInboundDID("5551234567")
	if got.E164 != "+15551234567" {
		t.Errorf("override CC: got %q, want +15551234567", got.E164)
	}

	// Empty country code falls back to +39 default.
	m.CountryCode = ""
	got = m.NormalizeInboundDID("123")
	if got.E164 != "+39123" {
		t.Errorf("empty CC fallback: got %q, want +39123", got.E164)
	}
}

func TestMessageNet_KeepaliveCadence(t *testing.T) {
	t.Parallel()
	if got := NewMessageNet().OptionsKeepaliveInterval(); got != 25*time.Second {
		t.Errorf("messagenet keepalive = %v, want 25s", got)
	}
}

func TestMessageNet_ContactRewriteIsNoop(t *testing.T) {
	t.Parallel()
	m := NewMessageNet()
	c := &ContactHints{User: "alice", Host: "example.com", Port: 5060}
	m.RewriteContactForRegister(c)
	if len(c.Params) != 0 {
		t.Errorf("MessageNet contact rewrite added params: %v", c.Params)
	}
	m.RewriteContactForRegister(nil) // nil-safe
}

func TestThreeCX_AddsTransportAndRinstance(t *testing.T) {
	t.Parallel()
	x := New3CX()
	c := &ContactHints{User: "100", Host: "pbx.example.com", Port: 5060}
	x.RewriteContactForRegister(c)

	if c.Params["transport"] != "udp" {
		t.Errorf("transport = %q, want udp", c.Params["transport"])
	}
	rinst := c.Params["rinstance"]
	if len(rinst) != 16 {
		t.Errorf("rinstance length = %d, want 16 hex chars (got %q)", len(rinst), rinst)
	}

	// Stability: a second call returns the same rinstance for the same UA.
	c2 := &ContactHints{User: "100", Host: "pbx.example.com", Port: 5060}
	x.RewriteContactForRegister(c2)
	if c2.Params["rinstance"] != rinst {
		t.Errorf("rinstance not stable: got %q vs %q", c2.Params["rinstance"], rinst)
	}

	// Distinctness: a different user@host yields a different rinstance.
	c3 := &ContactHints{User: "200", Host: "pbx.example.com", Port: 5060}
	x.RewriteContactForRegister(c3)
	if c3.Params["rinstance"] == rinst {
		t.Errorf("rinstance not distinct across UAs: %q", rinst)
	}
}

func TestThreeCX_RespectsCallerSetParams(t *testing.T) {
	t.Parallel()
	x := New3CX()
	c := &ContactHints{
		User: "100", Host: "pbx", Port: 5060,
		Params: map[string]string{
			"transport": "tcp", // caller wants TCP
			"rinstance": "deadbeefdeadbeef",
		},
	}
	x.RewriteContactForRegister(c)
	if c.Params["transport"] != "tcp" {
		t.Errorf("caller's transport overridden: %q", c.Params["transport"])
	}
	if c.Params["rinstance"] != "deadbeefdeadbeef" {
		t.Errorf("caller's rinstance overridden: %q", c.Params["rinstance"])
	}
}

func TestThreeCX_DIDPassthrough(t *testing.T) {
	t.Parallel()
	x := New3CX()
	got := x.NormalizeInboundDID("100")
	if got.E164 != "100" || got.Raw != "100" {
		t.Errorf("3CX NormalizeInboundDID = %+v, want both fields %q", got, "100")
	}
}

func TestThreeCX_NilContactSafe(t *testing.T) {
	t.Parallel()
	New3CX().RewriteContactForRegister(nil)
}

func TestThreeCX_KeepaliveCadence(t *testing.T) {
	t.Parallel()
	if got := New3CX().OptionsKeepaliveInterval(); got != 5*time.Minute {
		t.Errorf("3cx keepalive = %v, want 5m", got)
	}
}

func TestAllDigitsHelper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"0", true},
		{"123", true},
		{"123a", false},
		{"+123", false},
		{"1 2 3", false},
	}
	for _, c := range cases {
		if got := allDigits(c.in); got != c.want {
			t.Errorf("allDigits(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
