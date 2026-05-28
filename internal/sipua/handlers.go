package sipua

import (
	"sync"

	"github.com/emiago/sipgo/sip"
)

// callTable indexes in-flight calls by Call-ID for BYE/ACK correlation and
// shutdown teardown. The zero value is usable.
type callTable struct {
	mu sync.Mutex
	m  map[string]*Call
}

func (t *callTable) put(id string, c *Call) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*Call{}
	}
	t.m[id] = c
}

func (t *callTable) get(id string) *Call {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[id]
}

func (t *callTable) drop(id string) *Call {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.m[id]
	delete(t.m, id)
	return c
}

// snapshot returns a copy of all active calls without holding the lock.
func (t *callTable) snapshot() []*Call {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Call, 0, len(t.m))
	for _, c := range t.m {
		out = append(out, c)
	}
	return out
}

func (s *Server) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := ""
	if h := req.CallID(); h != nil {
		callID = h.Value()
	}
	s.logger.Info("INVITE received", "call_id", callID, "from", uriString(req.From()), "to", uriString(req.To()))

	// Send 100 Trying immediately so the registrar/SBC doesn't retransmit.
	if err := tx.Respond(sip.NewResponseFromRequest(req, 100, "Trying", nil)); err != nil {
		s.logger.Warn("send 100 Trying failed", "error", err)
	}

	c := &Call{
		CallID:    callID,
		RemoteSDP: req.Body(),
		srv:       s,
		req:       req,
		tx:        tx,
	}
	if f := req.From(); f != nil {
		c.From = f.Address
	}
	if t := req.To(); t != nil {
		c.To = t.Address
		// Surface the called DID via the active provider so downstream code
		// never branches on which registrar delivered the call. The provider
		// defaults to generic / pass-through when none was configured.
		if s.provider != nil {
			c.LocalDID = s.provider.NormalizeInboundDID(t.Address.User)
		}
	}

	s.calls.put(callID, c)

	if s.inviteHandler == nil {
		s.logger.Warn("no InviteHandler set; declining call")
		_ = c.Reply(488, "Not Acceptable Here", nil)
		s.calls.drop(callID)
		return
	}

	// Send 180 Ringing to give the caller audible feedback while the app
	// builds its answer.
	if err := tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil)); err != nil {
		s.logger.Warn("send 180 Ringing failed", "error", err)
	}

	// Run the user handler in-band so back-pressure is honoured. sipgo
	// dispatches each request on its own goroutine.
	s.inviteHandler(c)

	// If the handler returned without replying, fail loudly.
	c.mu.Lock()
	replied := c.replied
	c.mu.Unlock()
	if !replied {
		s.logger.Error("InviteHandler returned without Reply; sending 500")
		_ = c.Reply(500, "Server Internal Error", nil)
	}
}

func (s *Server) handleAck(req *sip.Request, _ sip.ServerTransaction) {
	callID := ""
	if h := req.CallID(); h != nil {
		callID = h.Value()
	}
	s.logger.Info("ACK received", "call_id", callID)
	// ACK has no response. The dialog is now confirmed; media (S04) starts
	// here when wired.
}

func (s *Server) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := ""
	if h := req.CallID(); h != nil {
		callID = h.Value()
	}
	s.logger.Info("BYE received", "call_id", callID)

	if err := tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil)); err != nil {
		s.logger.Warn("send 200 OK to BYE failed", "error", err)
	}

	if c := s.calls.drop(callID); c != nil {
		c.fireBye()
	}
}

// handleOptions replies 200 OK to OPTIONS keepalives. If the Call-ID belongs
// to a known dialog, the response is logged as in-dialog so operators can
// see registrar keepalives separately from mid-dialog probes.
func (s *Server) handleOptions(req *sip.Request, tx sip.ServerTransaction) {
	callID := ""
	if h := req.CallID(); h != nil {
		callID = h.Value()
	}
	inDialog := s.calls.get(callID) != nil

	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS"))
	if err := tx.Respond(res); err != nil {
		s.logger.Warn("send 200 OK to OPTIONS failed", "error", err)
		return
	}
	s.logger.Debug("OPTIONS keepalive", "call_id", callID, "in_dialog", inDialog)
}

func uriString(h sip.Header) string {
	if h == nil {
		return ""
	}
	return h.Value()
}
