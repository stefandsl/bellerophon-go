// Package sipprov defines the pluggable SIP provider layer that the
// Bellerophon UA uses to vary per-registrar behavior (DID normalization,
// OPTIONS keepalive cadence, Contact-header quirks). The package exposes
// only data and small synchronous methods — nothing here touches the
// network or sipgo types directly, so the providers are unit-testable
// without any SIP stack at all.
//
// Three providers ship in M001:
//   - generic     — RFC 3261 baseline, Asterisk-compatible.
//   - messagenet  — the user's actual ITSP for inbound Italian DIDs.
//   - 3cx         — legacy compatibility for Stefan's existing PBX.
//
// New providers add a file + a Get() registry case.
package sipprov

import (
	"fmt"
	"net/http"
	"time"
)

// Kind identifies a provider preset by its config-file name.
type Kind string

const (
	KindGeneric    Kind = "generic"
	KindMessageNet Kind = "messagenet"
	Kind3CX        Kind = "3cx"
)

// LocalDID is the called identifier extracted from an inbound INVITE's
// To: URI, normalized into both an E.164 form (preferred for downstream
// routing) and a Raw form (as it appeared on the wire). For SIP-extension
// providers like 3CX the two are usually identical; for ITSP DIDs they
// differ — MessageNet drops the country code, for example.
type LocalDID struct {
	E164 string // canonical e.g. "+390123456789"
	Raw  string // as the provider delivered it, e.g. "0123456789"
}

// ContactHints describes a Contact: header URI for RewriteContactForRegister.
// Using a struct rather than sip.Uri directly keeps this package free of
// any dependency on sipgo, which is what makes the provider quirks
// unit-testable in isolation. The SIP integration layer converts to/from
// sip.Uri at the boundary.
type ContactHints struct {
	User   string
	Host   string
	Port   int
	Params map[string]string // ;name=value tokens after the URI
}

// Provider abstracts the per-registrar behavior. Implementations should be
// stateless or carry only configuration; the SIP UA constructs them once
// and calls into them repeatedly.
type Provider interface {
	// Kind returns this provider's preset identifier.
	Kind() Kind

	// RegisterHeaders returns extra HTTP-style headers to inject on
	// REGISTER. Most providers return nil. Returning a non-nil map is a
	// pre-auth hint — the SIP UA folds these into the outgoing message
	// before sending. Header semantics are SIP-not-HTTP; http.Header is
	// used purely as a convenient bag.
	RegisterHeaders() http.Header

	// NormalizeInboundDID maps the user portion of an inbound INVITE's
	// To: URI (e.g. "0123456789" from MessageNet, "100" from 3CX) into a
	// normalized LocalDID. The conversation loop never branches on
	// provider — it consumes LocalDID.E164.
	NormalizeInboundDID(userOrPhone string) LocalDID

	// OptionsKeepaliveInterval is how often we should send OPTIONS pings
	// to the registrar to keep NAT bindings alive and detect dead
	// registrars. Zero means no keepalive needed (rare).
	OptionsKeepaliveInterval() time.Duration

	// RewriteContactForRegister mutates a Contact-header URI in-place to
	// match what the registrar expects on REGISTER. Most providers leave
	// it alone; 3CX wants ;transport=udp and a stable ;rinstance=.
	// nil-safe.
	RewriteContactForRegister(contact *ContactHints)
}

// Get returns the Provider implementation for kind. Unknown kinds error.
func Get(kind Kind) (Provider, error) {
	switch kind {
	case KindGeneric:
		return NewGeneric(), nil
	case KindMessageNet:
		return NewMessageNet(), nil
	case Kind3CX:
		return New3CX(), nil
	default:
		return nil, fmt.Errorf("sipprov: unknown kind %q (known: generic, messagenet, 3cx)", kind)
	}
}
