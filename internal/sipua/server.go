// Package sipua wraps the sipgo UAC/UAS into a single Bellerophon-flavoured
// Server: it REGISTERs against the configured registrar, keeps registration
// fresh, accepts inbound INVITEs through a user-supplied handler, and
// unregisters cleanly on shutdown.
//
// Media is intentionally out of scope here — INVITEs are answered with a
// caller-supplied SDP string (S04 wires the real RTP session).
package sipua

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"

	"github.com/stefandsl/bellerophon-go/internal/config"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
	"github.com/stefandsl/bellerophon-go/internal/sipprov"
)

// InviteHandler is invoked once per accepted inbound INVITE. It must call
// Call.Reply exactly once with the final response code (typically 200 with a
// real SDP, or a 4xx/5xx). It may register Call.OnBye to react to teardown.
type InviteHandler func(c *Call)

// Server is the high-level SIP user agent.
type Server struct {
	cfg    config.SIP
	local  string // ip:port we listen on
	logger bellog.Logger

	ua     *sipgo.UserAgent
	client *sipgo.Client
	srv    *sipgo.Server

	inviteHandler InviteHandler

	provider sipprov.Provider

	calls callTable

	mu         sync.Mutex
	registered bool
	// Sticky registration state. RFC 3261 §10.2 requires REGISTER refreshes
	// for the same address-of-record to use the same Call-ID with a
	// monotonically increasing CSeq. We also cache the digest challenge so
	// refreshes don't pay a 401 round-trip every cycle.
	regCallID    string
	regCSeq      uint32
	regContact   string
	regChal      *digest.Challenge
	regChalCount int

	stopRefresh chan struct{}
	wg          sync.WaitGroup
}

// Options configures NewServer.
type Options struct {
	// LocalAddr is the host:port the UA listens on. If host is empty it
	// defaults to 0.0.0.0; if port is 0 it defaults to 5060.
	LocalAddr string
	// Logger is used for all transitions. Required.
	Logger bellog.Logger
	// Provider supplies per-registrar quirks (DID normalization, OPTIONS
	// cadence, Contact rewrites). Nil means generic / RFC 3261 baseline.
	Provider sipprov.Provider
}

// NewServer wires sipgo together for the given config.
func NewServer(cfg config.SIP, opts Options) (*Server, error) {
	if opts.Logger == nil {
		return nil, errors.New("sipua: Logger is required")
	}
	local := opts.LocalAddr
	if local == "" {
		local = "0.0.0.0:5060"
	} else if !strings.Contains(local, ":") {
		local = local + ":5060"
	}

	username := cfg.AuthUsername
	if username == "" {
		username = cfg.Extension
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("bellerophon/0.1"))
	if err != nil {
		return nil, fmt.Errorf("sipua: new ua: %w", err)
	}

	host, _, splitErr := net.SplitHostPort(local)
	if splitErr != nil {
		host = local
	}

	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname(host))
	if err != nil {
		return nil, fmt.Errorf("sipua: new client: %w", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return nil, fmt.Errorf("sipua: new server: %w", err)
	}

	provider := opts.Provider
	if provider == nil {
		provider = sipprov.NewGeneric()
	}

	s := &Server{
		cfg:         cfg,
		local:       local,
		logger:      opts.Logger.With("component", "sipua"),
		ua:          ua,
		client:      client,
		srv:         srv,
		provider:    provider,
		stopRefresh: make(chan struct{}),
		regCallID:   randomCallID(),
		regCSeq:     1,
	}
	s.regContact = s.contactURI()

	// Use username as auth identity for log clarity.
	s.logger = s.logger.With("extension", cfg.Extension, "auth_user", username)

	s.srv.OnInvite(s.handleInvite)
	s.srv.OnAck(s.handleAck)
	s.srv.OnBye(s.handleBye)
	s.srv.OnOptions(s.handleOptions)

	return s, nil
}

// OnInvite sets the application callback fired for each accepted INVITE.
// Must be set before Run.
func (s *Server) OnInvite(h InviteHandler) { s.inviteHandler = h }

// Run starts listening, performs the initial REGISTER, and blocks until ctx
// is cancelled. On cancellation it unregisters (best-effort) and shuts down
// the transport layer.
func (s *Server) Run(ctx context.Context) error {
	// Start the UDP listener in a goroutine; sipgo's ListenAndServe blocks.
	serveErr := make(chan error, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.logger.Info("listening", "addr", s.local, "transport", "udp")
		err := s.srv.ListenAndServe(ctx, "udp", s.local)
		if err != nil && !errors.Is(err, context.Canceled) {
			serveErr <- err
		}
		close(serveErr)
	}()

	// Give the listener a moment to bind before REGISTER tries to use it.
	time.Sleep(50 * time.Millisecond)

	if err := s.register(ctx, s.cfg.Expiry); err != nil {
		s.logger.Error("initial REGISTER failed", "error", err)
		_ = s.client.Close()
		return fmt.Errorf("sipua: initial register: %w", err)
	}
	s.logger.Info("registered", "expires_s", s.cfg.Expiry)

	s.wg.Add(1)
	go s.refreshLoop(ctx)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("sipua: serve: %w", err)
		}
	}

	s.shutdown()
	s.wg.Wait()
	return nil
}

func (s *Server) shutdown() {
	close(s.stopRefresh)

	// Tear down any in-flight calls before deregistering so the registrar
	// sees us go offline cleanly and remote parties hear an immediate
	// hangup rather than waiting for their own timeout. Best-effort,
	// parallel, capped at 2s total.
	s.byeActiveCalls(2 * time.Second)

	// Best-effort unregister with Expires:0. Use a short timeout so a dead
	// registrar can't pin us in shutdown forever.
	uctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.register(uctx, 0); err != nil {
		s.logger.Warn("unregister failed", "error", err)
	} else {
		s.logger.Info("unregistered")
	}

	_ = s.client.Close()
	_ = s.srv.Close()
}

// byeActiveCalls walks the call table and sends BYE for each active call in
// parallel, waiting up to total for all of them. Calls are dropped from the
// table whether or not the BYE succeeds.
func (s *Server) byeActiveCalls(total time.Duration) {
	active := s.calls.snapshot()
	if len(active) == 0 {
		return
	}
	s.logger.Info("BYEing active calls on shutdown", "count", len(active))

	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()

	var wg sync.WaitGroup
	for _, c := range active {
		wg.Add(1)
		go func(c *Call) {
			defer wg.Done()
			if err := c.sendBye(ctx); err != nil {
				s.logger.Warn("outbound BYE failed", "call_id", c.CallID, "error", err)
			}
			s.calls.drop(c.CallID)
			c.fireBye()
		}(c)
	}
	wg.Wait()
}

func (s *Server) refreshLoop(ctx context.Context) {
	defer s.wg.Done()
	// Refresh at 50% of the expiry, minimum 30s.
	interval := time.Duration(s.cfg.Expiry) * time.Second / 2
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopRefresh:
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := s.register(rctx, s.cfg.Expiry); err != nil {
				s.logger.Warn("refresh REGISTER failed", "error", err)
			} else {
				s.logger.Debug("registration refreshed")
			}
			cancel()
		}
	}
}

// register sends a REGISTER request, transparently handling a 401 digest
// challenge. expirySec=0 deregisters.
//
// All REGISTERs reuse Server.regCallID and a monotonic Server.regCSeq, and
// reuse the cached digest challenge (Server.regChal) so refreshes don't pay
// a 401 round-trip after the first successful auth.
func (s *Server) register(ctx context.Context, expirySec int) error {
	registrar := s.cfg.Registrar
	if registrar == "" {
		registrar = s.cfg.Domain
	}
	port := s.cfg.RegistrarPort
	if port == 0 {
		port = 5060
	}
	registrarHostPort := fmt.Sprintf("%s:%d", registrar, port)

	var recipient sip.Uri
	if err := sip.ParseUri(fmt.Sprintf("sip:%s@%s", s.cfg.Extension, registrarHostPort), &recipient); err != nil {
		return fmt.Errorf("parse registrar uri: %w", err)
	}

	// Build a REGISTER request with our sticky Call-ID and the next CSeq.
	// sipgo's ClientRequestRegisterBuild will increment the CSeq once more
	// before sending; we read the final value back to keep our counter in
	// sync.
	req, nextCSeq := s.buildRegisterRequest(recipient, expirySec)

	username := s.cfg.AuthUsername
	if username == "" {
		username = s.cfg.Extension
	}

	// If we have a cached challenge from a prior 401, pre-attach the
	// Authorization header so the registrar accepts on the first round.
	s.mu.Lock()
	chal := s.regChal
	if chal != nil {
		s.regChalCount++
	}
	chalCount := s.regChalCount
	s.mu.Unlock()

	if chal != nil {
		if err := attachDigest(req, recipient, chal, chalCount, username, s.cfg.AuthPassword); err != nil {
			s.logger.Warn("attach cached digest failed; falling back to challenge round-trip", "error", err)
		}
	}

	tx, err := s.client.TransactionRequest(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return fmt.Errorf("create REGISTER tx: %w", err)
	}
	defer tx.Terminate()

	res, err := awaitResponse(ctx, tx)
	if err != nil {
		return err
	}

	// Update sticky CSeq from what was actually sent.
	if cs := req.CSeq(); cs != nil {
		s.mu.Lock()
		s.regCSeq = cs.SeqNo
		s.mu.Unlock()
	} else {
		s.mu.Lock()
		s.regCSeq = nextCSeq
		s.mu.Unlock()
	}

	if res.StatusCode == 401 || res.StatusCode == 407 {
		hdr := res.GetHeader("WWW-Authenticate")
		if hdr == nil {
			hdr = res.GetHeader("Proxy-Authenticate")
		}
		if hdr == nil {
			return fmt.Errorf("auth required but no challenge header")
		}
		newChal, err := digest.ParseChallenge(hdr.Value())
		if err != nil {
			return fmt.Errorf("parse digest challenge: %w", err)
		}

		// Cache the new challenge; reset nonce-count.
		s.mu.Lock()
		s.regChal = newChal
		s.regChalCount = 1
		newCount := s.regChalCount
		s.mu.Unlock()

		newReq := req.Clone()
		newReq.RemoveHeader("Via")
		newReq.RemoveHeader("Authorization")
		newReq.RemoveHeader("Proxy-Authorization")
		if err := attachDigestNamed(newReq, recipient, newChal, newCount, username, s.cfg.AuthPassword, res.StatusCode == 407); err != nil {
			return fmt.Errorf("compute digest: %w", err)
		}

		tx2, err := s.client.TransactionRequest(ctx, newReq, sipgo.ClientRequestIncreaseCSEQ, sipgo.ClientRequestAddVia)
		if err != nil {
			return fmt.Errorf("create authed REGISTER tx: %w", err)
		}
		defer tx2.Terminate()
		res, err = awaitResponse(ctx, tx2)
		if err != nil {
			return err
		}

		if cs := newReq.CSeq(); cs != nil {
			s.mu.Lock()
			s.regCSeq = cs.SeqNo
			s.mu.Unlock()
		}
	}

	if res.StatusCode != 200 {
		return fmt.Errorf("REGISTER got %d %s", res.StatusCode, res.Reason)
	}

	s.mu.Lock()
	s.registered = expirySec > 0
	s.mu.Unlock()
	return nil
}

// buildRegisterRequest produces a fresh REGISTER with the sticky Call-ID and
// the next sticky CSeq pre-attached. The returned nextCSeq is what we set on
// the request; sipgo's REGISTER builder will bump it by one more before
// sending.
func (s *Server) buildRegisterRequest(recipient sip.Uri, expirySec int) (*sip.Request, uint32) {
	req := sip.NewRequest(sip.REGISTER, recipient)
	req.AppendHeader(sip.NewHeader("Contact", s.regContact))
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(expirySec)))
	req.SetTransport("UDP")

	s.mu.Lock()
	callID := sip.CallIDHeader(s.regCallID)
	nextCSeq := s.regCSeq + 1
	s.mu.Unlock()

	req.AppendHeader(&callID)
	cseq := &sip.CSeqHeader{SeqNo: nextCSeq, MethodName: sip.REGISTER}
	req.AppendHeader(cseq)

	return req, nextCSeq
}

// attachDigest computes a digest credential and appends it as the
// Authorization header. Used when re-using a cached challenge.
func attachDigest(req *sip.Request, recipient sip.Uri, chal *digest.Challenge, count int, username, password string) error {
	return attachDigestNamed(req, recipient, chal, count, username, password, false)
}

// attachDigestNamed appends Authorization or Proxy-Authorization depending on
// whether the challenge was a 407.
func attachDigestNamed(req *sip.Request, recipient sip.Uri, chal *digest.Challenge, count int, username, password string, proxy bool) error {
	cred, err := digest.Digest(chal, digest.Options{
		Method:   req.Method.String(),
		URI:      recipient.Host,
		Username: username,
		Password: password,
		Count:    count,
	})
	if err != nil {
		return err
	}
	hdr := "Authorization"
	if proxy {
		hdr = "Proxy-Authorization"
	}
	req.AppendHeader(sip.NewHeader(hdr, cred.String()))
	return nil
}

// contactURI builds the Contact header value advertised to the registrar.
func (s *Server) contactURI() string {
	host, port, err := net.SplitHostPort(s.local)
	if err != nil {
		host = s.local
		port = "5060"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = firstNonLoopbackIP()
	}
	return fmt.Sprintf("<sip:%s@%s:%s;transport=udp>", s.cfg.Extension, host, port)
}

// awaitResponse waits for a final response on the transaction or ctx
// cancellation.
func awaitResponse(ctx context.Context, tx sip.ClientTransaction) (*sip.Response, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tx.Done():
			return nil, errors.New("transaction terminated without final response")
		case res := <-tx.Responses():
			if res == nil {
				return nil, errors.New("nil response")
			}
			if res.StatusCode >= 200 {
				return res, nil
			}
			// keep waiting on provisional
		}
	}
}

// firstNonLoopbackIP returns the first non-loopback IPv4 found on the host,
// or "127.0.0.1" if none. Used as a fallback Contact host when listening on
// 0.0.0.0 and no RTP.ExternalIP override is supplied.
func firstNonLoopbackIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() {
			continue
		}
		v4 := ipn.IP.To4()
		if v4 != nil {
			return v4.String()
		}
	}
	return "127.0.0.1"
}

// randomCallID returns a random 16-byte hex string suitable as a Call-ID.
func randomCallID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-based pseudo-id; collision risk is acceptable
		// for a single-extension UA.
		return fmt.Sprintf("bellerophon-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:]) + "@bellerophon"
}
