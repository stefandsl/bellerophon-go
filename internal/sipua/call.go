package sipua

import (
	"context"
	"fmt"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/stefandsl/bellerophon-go/internal/sipprov"
)

// Call represents an in-flight INVITE dialog. It is delivered to the
// InviteHandler; the handler must call Reply exactly once with the final
// response status.
type Call struct {
	From      sip.Uri
	To        sip.Uri
	CallID    string
	RemoteSDP []byte
	// LocalDID is the called identifier extracted from the To: URI and
	// normalized through the active sipprov.Provider. The conversation
	// loop (M002+) and the M003 multi-extension router consume
	// LocalDID.E164; LocalDID.Raw is preserved for logging / debugging.
	LocalDID sipprov.LocalDID

	srv *Server
	req *sip.Request
	tx  sip.ServerTransaction

	mu       sync.Mutex
	replied  bool
	onBye    func()
	finished bool
	// Local CSeq counter for in-dialog requests we originate (e.g. BYE
	// during shutdown). Independent of the remote party's CSeq stream.
	localCSeq uint32
}

// Reply sends a final response to the INVITE. sdp is the body bytes (typically
// an "application/sdp" body for a 200 OK); pass nil for non-200 responses.
func (c *Call) Reply(code int, reason string, sdp []byte) error {
	c.mu.Lock()
	if c.replied {
		c.mu.Unlock()
		return nil
	}
	c.replied = true
	c.mu.Unlock()

	res := sip.NewResponseFromRequest(c.req, code, reason, sdp)
	if len(sdp) > 0 {
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	// Set Contact so subsequent in-dialog requests (BYE) route back to us.
	res.AppendHeader(sip.NewHeader("Contact", c.srv.contactURI()))
	return c.tx.Respond(res)
}

// OnBye registers a callback fired when the remote party sends BYE. It is
// invoked exactly once.
func (c *Call) OnBye(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onBye = fn
}

func (c *Call) fireBye() {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	fn := c.onBye
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// sendBye sends a BYE to the remote party. Used during shutdown to tear down
// active dialogs cleanly. Best-effort: errors are returned but the call is
// considered finished regardless.
//
// Dialog construction here is intentionally minimal — we were the UAS for the
// INVITE so the BYE's From mirrors the original To (our address-of-record)
// and To mirrors the original From. The request-URI targets the remote
// Contact when available, falling back to the original From URI.
func (c *Call) sendBye(ctx context.Context) error {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return nil
	}
	if c.srv == nil || c.srv.client == nil {
		c.mu.Unlock()
		return fmt.Errorf("call has no client")
	}
	c.localCSeq++
	cseqNo := c.localCSeq
	c.mu.Unlock()

	// Request-URI: prefer the remote party's Contact (set on their 200 OK
	// or on the INVITE itself for early dialogs). Fallback to From URI.
	target := c.From
	if c.req != nil {
		if ct := c.req.Contact(); ct != nil {
			target = ct.Address
		}
	}

	bye := sip.NewRequest(sip.BYE, target)
	bye.SetTransport("UDP")

	// Call-ID matches the INVITE.
	callID := sip.CallIDHeader(c.CallID)
	bye.AppendHeader(&callID)

	// From = our side (the INVITE's To, including our tag if we set one).
	// To   = remote side (the INVITE's From, with their tag).
	if c.req != nil {
		if t := c.req.To(); t != nil {
			from := &sip.FromHeader{Address: t.Address, Params: t.Params}
			bye.AppendHeader(from)
		}
		if f := c.req.From(); f != nil {
			to := &sip.ToHeader{Address: f.Address, Params: f.Params}
			bye.AppendHeader(to)
		}
	}

	cseq := &sip.CSeqHeader{SeqNo: cseqNo, MethodName: sip.BYE}
	bye.AppendHeader(cseq)

	tx, err := c.srv.client.TransactionRequest(ctx, bye, sipgo.ClientRequestAddVia)
	if err != nil {
		return fmt.Errorf("BYE tx: %w", err)
	}
	defer tx.Terminate()

	res, err := awaitResponse(ctx, tx)
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("BYE got %d %s", res.StatusCode, res.Reason)
	}
	return nil
}
