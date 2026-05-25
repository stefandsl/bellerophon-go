package sipua

import (
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Call represents an in-flight INVITE dialog. It is delivered to the
// InviteHandler; the handler must call Reply exactly once with the final
// response status.
type Call struct {
	From      sip.Uri
	To        sip.Uri
	CallID    string
	RemoteSDP []byte

	srv *Server
	req *sip.Request
	tx  sip.ServerTransaction

	mu       sync.Mutex
	replied  bool
	onBye    func()
	finished bool
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
