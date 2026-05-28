package sipprov

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"time"
)

// ThreeCXProvider is the legacy provider for the 3CX PBX. Key behavioral
// differences from generic:
//
//   - The 3CX SBC enforces ;transport= on the Contact header — without it,
//     in-dialog requests (BYE, re-INVITE) round-trip with the wrong
//     transport and get dropped.
//   - 3CX uses ;rinstance= as a stable per-extension binding identifier;
//     without one it may evict our registration when it cleans up stale
//     entries from a previous registration.
//   - A 3CX extension goes "offline" after ~10 min without any traffic.
//     Sending OPTIONS every 5 min is conservative and well under the cap.
//
// DID normalization is a no-op: 3CX delivers extension numbers verbatim
// (e.g. "100"), which is already the canonical form for in-PBX routing.
type ThreeCXProvider struct{}

// New3CX returns a 3CX provider.
func New3CX() *ThreeCXProvider { return &ThreeCXProvider{} }

func (*ThreeCXProvider) Kind() Kind                            { return Kind3CX }
func (*ThreeCXProvider) RegisterHeaders() http.Header          { return nil }
func (*ThreeCXProvider) NormalizeInboundDID(s string) LocalDID { return LocalDID{E164: s, Raw: s} }

// 3CX disconnects extensions after ~10 min of silence. 5-minute OPTIONS
// pings stay safely below that and double as an early dead-registrar
// detector if 3CX itself goes down.
func (*ThreeCXProvider) OptionsKeepaliveInterval() time.Duration { return 5 * time.Minute }

// RewriteContactForRegister adds the two parameters 3CX requires:
//   - transport=udp (matches our wire transport; without this 3CX may
//     route in-dialog requests over TCP and we drop the response)
//   - rinstance=<stable 64-bit hex hash of user@host> (gives 3CX a stable
//     identifier across re-REGISTER cycles so old bindings aren't evicted)
//
// Both are added only if absent — if the caller already set them we
// respect that. Safe to call on a nil Contact (no-op).
func (*ThreeCXProvider) RewriteContactForRegister(c *ContactHints) {
	if c == nil {
		return
	}
	if c.Params == nil {
		c.Params = map[string]string{}
	}
	if _, ok := c.Params["transport"]; !ok {
		c.Params["transport"] = "udp"
	}
	if _, ok := c.Params["rinstance"]; !ok {
		c.Params["rinstance"] = rinstanceFor(c.User + "@" + c.Host)
	}
}

// rinstanceFor returns a stable 16-character hex identifier derived from
// the seed. 3CX wants this stable across REGISTERs from the same UA, so
// hashing user@host gives us a deterministic value without needing to
// persist anything.
func rinstanceFor(seed string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	return fmt.Sprintf("%016x", h.Sum64())
}
