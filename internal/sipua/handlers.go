package sipua

import (
	"sync"

	"github.com/emiago/sipgo/sip"
)

// activeCalls indexes in-flight calls by Call-ID for BYE/ACK correlation.
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

func (t *callTable) drop(id string) *Call {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.m[id]
	delete(t.m, id)
	return c
}

var calls = &callTable{}

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
	}

	calls.put(callID, c)

	// Send 180 Ringing to give the caller audible feedback while the app
	// builds its answer.
	if err := tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil)); err != nil {
		s.logger.Warn("send 180 Ringing failed", "error", err)
	}

	if s.inviteHandler == nil {
		s.logger.Warn("no InviteHandler set; declining call")
		_ = c.Reply(488, "Not Acceptable Here", nil)
		calls.drop(callID)
		return
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

	if c := calls.drop(callID); c != nil {
		c.fireBye()
	}
}

func (s *Server) handleOptions(req *sip.Request, tx sip.ServerTransaction) {
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, BYE, CANCEL, OPTIONS"))
	if err := tx.Respond(res); err != nil {
		s.logger.Warn("send 200 OK to OPTIONS failed", "error", err)
	}
}

func uriString(h sip.Header) string {
	if h == nil {
		return ""
	}
	return h.Value()
}
