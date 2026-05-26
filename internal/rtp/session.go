// Package rtp owns Bellerophon's RTP media transport: a UDP socket pair
// (RTP + RTCP) per call, pion/rtp packet parsing/marshalling, and the
// jitter / RTCP / DTMF machinery that lands in later S04+ tasks.
//
// This file is S04 T01: the bare Session — open the socket inside the
// configured port range, parse inbound packets into pion/rtp.Packet values,
// and marshal outbound payloads onto the wire. Jitter buffering (T03),
// RTCP heartbeat (T04), and codec wiring (S05) build on top.
package rtp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

// DefaultPortRange is used when the parsed range is empty. Matches the
// FreeSWITCH range advertised in the v1 docker-compose so 3CX SBC ACLs
// continue to work without operator action.
const DefaultPortRange = "30000-30100"

// PortRange is an inclusive [Min, Max] UDP port window. RTP grabs an even
// port; the next odd port is reserved for RTCP per RFC 3550 §11.
type PortRange struct{ Min, Max int }

// ParsePortRange parses a "min-max" string. An empty input returns the
// default range. Both bounds must be in [1024, 65534]; Min must be even and
// Min < Max.
func ParsePortRange(s string) (PortRange, error) {
	if strings.TrimSpace(s) == "" {
		s = DefaultPortRange
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return PortRange{}, fmt.Errorf("rtp: port range %q: want MIN-MAX", s)
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return PortRange{}, fmt.Errorf("rtp: port range %q: bounds must be integers", s)
	}
	if lo < 1024 || hi > 65534 || lo >= hi {
		return PortRange{}, fmt.Errorf("rtp: port range %q: need 1024 <= MIN < MAX <= 65534", s)
	}
	if lo%2 != 0 {
		// RFC 3550 §11: RTP on even, RTCP on the next odd. Bump if the
		// operator gave us an odd MIN so we don't fail every attempt.
		lo++
	}
	if lo >= hi {
		return PortRange{}, fmt.Errorf("rtp: port range %q: too narrow after even-MIN adjustment", s)
	}
	return PortRange{Min: lo, Max: hi}, nil
}

// Packet is the parsed wire representation handed to consumers of a Session.
// It owns its Payload slice — the caller may retain it.
type Packet struct {
	Header  rtp.Header
	Payload []byte
}

// Options configures a Session.
type Options struct {
	// LocalIP is the address to bind the RTP/RTCP sockets to. Empty means
	// "0.0.0.0" (all interfaces). The IP advertised in SDP is a separate
	// concern owned by the SIP layer.
	LocalIP string
	// PortRange limits the RTP port selection. Zero value means
	// DefaultPortRange.
	PortRange PortRange
	// Logger is required.
	Logger bellog.Logger
	// RecvBufBytes sizes the inbound channel. Zero -> 256 packets (~5 s at
	// 20 ms ptime, generous head-room for the jitter buffer in T03).
	RecvBufBytes int
	// SSRC overrides the local synchronization source. Zero -> random.
	SSRC uint32
	// ClockRate is the RTP timestamp clock used for jitter math. Zero ->
	// 8000 (G.711). When the codec changes in S05 this becomes per-session.
	ClockRate uint32
	// Now is the clock source for RTCP timing. Zero -> time.Now. Injectable
	// for tests.
	Now func() time.Time
}

// Session is one RTP flow: an RTP UDP socket plus its paired RTCP socket on
// rtpPort+1, both bound at construction. The remote peer is set once the
// SDP answer is known (SetRemote).
type Session struct {
	logger bellog.Logger

	rtpConn  *net.UDPConn
	rtcpConn *net.UDPConn
	rtpPort  int

	remote atomic.Pointer[net.UDPAddr]

	recv chan Packet

	closeOnce sync.Once
	closed    chan struct{}

	// Stats. Read with the corresponding getters; atomics keep them
	// lock-free for the hot RX/TX paths.
	rxPackets atomic.Uint64
	txPackets atomic.Uint64
	rxBytes   atomic.Uint64
	txBytes   atomic.Uint64
	parseErrs atomic.Uint64

	// Identity + clock used by the RTCP reporter (T04).
	localSSRC uint32
	clockRate uint32
	now       func() time.Time

	// Last RTP timestamp sent (host order). Used for SR.
	lastTxTS atomic.Uint32

	// Inbound per-stream stats for the RTCP RR/SR block. Single peer SSRC
	// is the common case in our SIP world; if the peer changes mid-call we
	// reset.
	rxMu          sync.Mutex
	rxSSRC        uint32
	rxSSRCSet     bool
	rxBaseSeq     uint32 // extended
	rxMaxSeq      uint32 // extended
	rxRecv        uint32 // packets received in this SSRC stream
	rxJitter      float64
	rxTransitSet  bool
	rxLastTransit int64 // transit time in clock-rate units (signed for math)
	rxLastSeq     uint16
	rxCycles      uint32
	rxLastSRMid   uint32    // middle-32 of remote SR NTP
	rxLastSRWhen  time.Time // arrival of last SR
}

// NewSession binds an RTP/RTCP socket pair somewhere in opts.PortRange and
// starts the receive goroutine. The first usable even port wins; sockets
// already in use are skipped silently.
func NewSession(opts Options) (*Session, error) {
	if opts.Logger == nil {
		return nil, errors.New("rtp: Logger is required")
	}
	if opts.PortRange == (PortRange{}) {
		pr, err := ParsePortRange("")
		if err != nil {
			return nil, err
		}
		opts.PortRange = pr
	}
	bindIP := opts.LocalIP
	if bindIP == "" {
		bindIP = "0.0.0.0"
	}
	ip := net.ParseIP(bindIP)
	if ip == nil {
		return nil, fmt.Errorf("rtp: invalid LocalIP %q", bindIP)
	}

	rtpConn, rtcpConn, port, err := bindPair(ip, opts.PortRange)
	if err != nil {
		return nil, err
	}

	bufN := opts.RecvBufBytes
	if bufN <= 0 {
		bufN = 256
	}

	ssrc := opts.SSRC
	if ssrc == 0 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("rtp: random SSRC: %w", err)
		}
		ssrc = binary.BigEndian.Uint32(b[:])
		if ssrc == 0 {
			ssrc = 1
		}
	}
	clk := opts.ClockRate
	if clk == 0 {
		clk = 8000
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	s := &Session{
		logger:    opts.Logger.With("component", "rtp", "rtp_port", port),
		rtpConn:   rtpConn,
		rtcpConn:  rtcpConn,
		rtpPort:   port,
		recv:      make(chan Packet, bufN),
		closed:    make(chan struct{}),
		localSSRC: ssrc,
		clockRate: clk,
		now:       nowFn,
	}
	s.logger.Info("rtp session opened",
		"rtp_addr", rtpConn.LocalAddr().String(),
		"rtcp_addr", rtcpConn.LocalAddr().String(),
	)
	go s.readLoop()
	return s, nil
}

// LocalPort returns the bound RTP port. RTCP is always LocalPort()+1.
func (s *Session) LocalPort() int { return s.rtpPort }

// RTCPPort returns the bound RTCP port (always LocalPort()+1).
func (s *Session) RTCPPort() int { return s.rtpPort + 1 }

// SetRemote sets the destination for Send. Safe to change mid-call (e.g. on
// re-INVITE in a later milestone).
func (s *Session) SetRemote(host string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("rtp: remote port %d out of range", port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Resolve a hostname if the SDP carried one. SDP c= lines are
		// usually literal IPs but be defensive.
		addrs, err := net.LookupIP(host)
		if err != nil || len(addrs) == 0 {
			return fmt.Errorf("rtp: resolve remote %q: %w", host, err)
		}
		ip = addrs[0]
	}
	s.remote.Store(&net.UDPAddr{IP: ip, Port: port})
	s.logger.Info("rtp remote set", "host", ip.String(), "port", port)
	return nil
}

// Recv returns the receive channel. It is closed when the session closes.
func (s *Session) Recv() <-chan Packet { return s.recv }

// Send marshals and writes a single RTP packet to the remote peer. SetRemote
// must have been called first. The header is taken by value; the caller may
// mutate it (sequence number, timestamp) between sends.
func (s *Session) Send(h rtp.Header, payload []byte) error {
	dst := s.remote.Load()
	if dst == nil {
		return errors.New("rtp: remote not set; call SetRemote before Send")
	}
	pkt := rtp.Packet{Header: h, Payload: payload}
	buf, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("rtp: marshal: %w", err)
	}
	n, err := s.rtpConn.WriteToUDP(buf, dst)
	if err != nil {
		return fmt.Errorf("rtp: write %s: %w", dst, err)
	}
	s.txPackets.Add(1)
	s.txBytes.Add(uint64(n))
	s.lastTxTS.Store(h.Timestamp)
	return nil
}

// LocalSSRC returns the SSRC used for outbound RTP and RTCP from this session.
func (s *Session) LocalSSRC() uint32 { return s.localSSRC }

// Stats is a snapshot of session counters. Cheap to call; values are
// loaded with atomics.
type Stats struct {
	RxPackets, TxPackets uint64
	RxBytes, TxBytes     uint64
	ParseErrors          uint64
}

// Stats returns the current counters.
func (s *Session) Stats() Stats {
	return Stats{
		RxPackets:   s.rxPackets.Load(),
		TxPackets:   s.txPackets.Load(),
		RxBytes:     s.rxBytes.Load(),
		TxBytes:     s.txBytes.Load(),
		ParseErrors: s.parseErrs.Load(),
	}
}

// Close releases the UDP sockets and stops the read loop. Idempotent.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.rtpConn.Close()
		_ = s.rtcpConn.Close()
		s.logger.Info("rtp session closed", "stats", s.Stats())
	})
	return nil
}

// Done returns a channel closed when the session is closed.
func (s *Session) Done() <-chan struct{} { return s.closed }

// CloseWithContext closes the session when ctx is done; convenient for
// goroutine-leak-free wiring from the call lifecycle.
func (s *Session) CloseWithContext(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
		case <-s.closed:
		}
		_ = s.Close()
	}()
}

func (s *Session) readLoop() {
	defer close(s.recv)
	// MTU-safe buffer: G.711 20 ms = 172 bytes incl. RTP header; SRTP /
	// VP8 / etc. push higher but never past ~1500. 2 KiB is comfortable.
	buf := make([]byte, 2048)
	for {
		n, _, err := s.rtpConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			// On a real socket error after close, still exit. On a
			// transient error log and continue.
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			s.logger.Warn("rtp read error", "err", err.Error())
			return
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			s.parseErrs.Add(1)
			s.logger.Debug("rtp parse failed", "err", err.Error(), "bytes", n)
			continue
		}
		s.rxPackets.Add(1)
		s.rxBytes.Add(uint64(n))
		s.observeRx(pkt.Header.SSRC, pkt.Header.SequenceNumber, pkt.Header.Timestamp)
		payload := make([]byte, len(pkt.Payload))
		copy(payload, pkt.Payload)
		out := Packet{Header: pkt.Header, Payload: payload}
		select {
		case s.recv <- out:
		case <-s.closed:
			return
		default:
			// Slow consumer. Drop and count as a parse-style error
			// so the upstream jitter buffer (T03) can react. A
			// dedicated drop counter lands when the JB does.
			s.parseErrs.Add(1)
		}
	}
}

// observeRx updates per-stream inbound stats — sequence tracking, packet
// count, and the interarrival jitter from RFC 3550 §6.4.1. Called from the
// read loop on every parsed inbound RTP packet.
func (s *Session) observeRx(ssrc uint32, seq uint16, ts uint32) {
	now := s.now()
	s.rxMu.Lock()
	defer s.rxMu.Unlock()

	if !s.rxSSRCSet || s.rxSSRC != ssrc {
		// Fresh stream (first packet, or peer SSRC changed mid-call). Reset.
		s.rxSSRC = ssrc
		s.rxSSRCSet = true
		s.rxBaseSeq = uint32(seq)
		s.rxMaxSeq = uint32(seq)
		s.rxLastSeq = seq
		s.rxCycles = 0
		s.rxRecv = 1
		s.rxJitter = 0
		s.rxTransitSet = false
	} else {
		// 16-bit seq wrap detection.
		diff := int32(seq) - int32(s.rxLastSeq)
		if diff < -0x7FFF {
			s.rxCycles++
		}
		s.rxLastSeq = seq
		ext := (s.rxCycles << 16) | uint32(seq)
		if ext > s.rxMaxSeq {
			s.rxMaxSeq = ext
		}
		s.rxRecv++
	}

	// Interarrival jitter — RFC 3550 §6.4.1.
	// transit = arrival (in clock-rate ticks) - rtp timestamp
	arrivalTicks := int64(now.UnixNano()) * int64(s.clockRate) / int64(time.Second)
	transit := arrivalTicks - int64(ts)
	if s.rxTransitSet {
		d := transit - s.rxLastTransit
		if d < 0 {
			d = -d
		}
		s.rxJitter += (float64(d) - s.rxJitter) / 16.0
	}
	s.rxLastTransit = transit
	s.rxTransitSet = true
}

// rxSnapshot returns a copy of the inbound report state. Returns false when
// no inbound RTP has been observed yet.
type rxSnapshot struct {
	ssrc         uint32
	baseSeq      uint32
	maxSeq       uint32 // extended highest sequence
	received     uint32
	jitter       uint32
	lastSRMid    uint32
	lastSRWhen   time.Time
	expectedPrev uint32
	lostPrev     uint32
}

func (s *Session) rxSnap() (rxSnapshot, bool) {
	s.rxMu.Lock()
	defer s.rxMu.Unlock()
	if !s.rxSSRCSet {
		return rxSnapshot{}, false
	}
	return rxSnapshot{
		ssrc:       s.rxSSRC,
		baseSeq:    s.rxBaseSeq,
		maxSeq:     s.rxMaxSeq,
		received:   s.rxRecv,
		jitter:     uint32(s.rxJitter),
		lastSRMid:  s.rxLastSRMid,
		lastSRWhen: s.rxLastSRWhen,
	}, true
}

func (s *Session) noteRemoteSR(mid32 uint32, when time.Time) {
	s.rxMu.Lock()
	s.rxLastSRMid = mid32
	s.rxLastSRWhen = when
	s.rxMu.Unlock()
}

// bindPair walks the port range looking for a free even port for RTP whose
// odd successor is also free (for RTCP).
func bindPair(ip net.IP, pr PortRange) (*net.UDPConn, *net.UDPConn, int, error) {
	var lastErr error
	for p := pr.Min; p+1 <= pr.Max; p += 2 {
		rtpAddr := &net.UDPAddr{IP: ip, Port: p}
		rtpConn, err := net.ListenUDP("udp", rtpAddr)
		if err != nil {
			lastErr = err
			continue
		}
		rtcpAddr := &net.UDPAddr{IP: ip, Port: p + 1}
		rtcpConn, err := net.ListenUDP("udp", rtcpAddr)
		if err != nil {
			_ = rtpConn.Close()
			lastErr = err
			continue
		}
		return rtpConn, rtcpConn, p, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no candidates")
	}
	return nil, nil, 0, fmt.Errorf("rtp: no free port pair in %d-%d: %w", pr.Min, pr.Max, lastErr)
}
