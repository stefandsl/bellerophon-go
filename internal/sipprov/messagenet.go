package sipprov

import (
	"net/http"
	"strings"
	"time"
)

// MessageNetProvider is the ITSP-side provider for messagenet.it. Key
// behavioral differences from generic:
//
//   - Inbound DIDs arrive in the To: URI without the country code (e.g.
//     "0123456789"). NormalizeInboundDID prepends the configured country
//     code so downstream routing sees a single E.164 form.
//   - The registrar sends OPTIONS pings periodically; we should respond
//     and also send our own at a matching cadence (~25 s) so the binding
//     stays alive.
//
// REGISTER auth flow is the standard 401-challenge dance — no special
// header injection needed (despite some older docs claiming otherwise).
type MessageNetProvider struct {
	// CountryCode is prepended to inbound DIDs that don't already start
	// with "+". Defaults to "+39" (Italy) — MessageNet is an Italian ITSP.
	CountryCode string
}

// NewMessageNet returns a MessageNet provider with the +39 default.
func NewMessageNet() *MessageNetProvider {
	return &MessageNetProvider{CountryCode: "+39"}
}

func (*MessageNetProvider) Kind() Kind                   { return KindMessageNet }
func (*MessageNetProvider) RegisterHeaders() http.Header { return nil }

// NormalizeInboundDID handles the MessageNet quirk: DIDs land without the
// country code, and Italian numbers often (but not always) carry a leading
// 0 trunk digit that should be stripped before adding +39.
func (m *MessageNetProvider) NormalizeInboundDID(s string) LocalDID {
	raw := s
	e164 := s

	if !strings.HasPrefix(e164, "+") && len(e164) > 0 && allDigits(e164) {
		cc := m.CountryCode
		if cc == "" {
			cc = "+39"
		}
		// Italian national numbers may have a leading trunk 0; strip it
		// before prepending the country code.
		e164 = cc + strings.TrimPrefix(e164, "0")
	}
	return LocalDID{E164: e164, Raw: raw}
}

// MessageNet declares a binding stale after roughly 30 s without
// activity. 25 s gives us comfortable headroom for one round-trip retry.
func (*MessageNetProvider) OptionsKeepaliveInterval() time.Duration { return 25 * time.Second }

// MessageNet accepts any RFC-3261-compliant Contact; no rewrite needed.
func (*MessageNetProvider) RewriteContactForRegister(_ *ContactHints) {}

// allDigits reports whether s is non-empty and contains only ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
