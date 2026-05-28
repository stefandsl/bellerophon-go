package sipprov

import (
	"net/http"
	"time"
)

// genericProvider is the RFC 3261 baseline implementation. It applies no
// per-registrar quirks — what comes in on the wire is what the rest of the
// stack sees. Use this for self-hosted Asterisk / FreeSWITCH and as the
// default for any registrar without a dedicated preset.
type genericProvider struct{}

// NewGeneric returns the baseline provider.
func NewGeneric() Provider { return &genericProvider{} }

func (genericProvider) Kind() Kind                              { return KindGeneric }
func (genericProvider) RegisterHeaders() http.Header            { return nil }
func (genericProvider) NormalizeInboundDID(s string) LocalDID   { return LocalDID{E164: s, Raw: s} }
func (genericProvider) OptionsKeepaliveInterval() time.Duration { return 30 * time.Second }
func (genericProvider) RewriteContactForRegister(_ *ContactHints) {
	// No-op — generic-RFC-3261 registrars accept whatever Contact we emit.
}
