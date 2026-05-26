// RTCP SR/RR heartbeat for S04 T04.
//
// Per RFC 3550 §6.4 every active RTP session emits compound RTCP packets on
// rtp_port+1 at a steady interval. The full RFC bandwidth-share algorithm is
// overkill for a single point-to-point SIP call, so this implementation does
// the pragmatic thing the spec demands: fire every 5 s, emit an SR when we
// have sent any RTP, else an RR when we have received any, and skip the tick
// entirely while the session is idle in both directions.
//
// Inbound RTCP is parsed for the remote SR's NTP middle-32 so the next
// outbound report's DLSR field is meaningful — that's what lets the peer
// compute round-trip time per §6.4.1.
package rtp

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// DefaultRTCPInterval is the heartbeat period from M001-SPEC.md §S04.
const DefaultRTCPInterval = 5 * time.Second

// ReporterOptions configures a Reporter.
type ReporterOptions struct {
	// Interval is the heartbeat period. Zero -> DefaultRTCPInterval.
	Interval time.Duration
	// CNAME is the canonical end-point identifier carried in the SDES
	// packet. Empty -> "bellerophon@<local-ip>:<rtp-port>".
	CNAME string
}

// Reporter drives the 5-second RTCP heartbeat for a Session. It owns the
// inbound RTCP read goroutine and the tick goroutine; both stop when the
// Session closes or Start's context is cancelled.
type Reporter struct {
	sess     *Session
	interval time.Duration
	cname    string

	startOnce sync.Once
	stopOnce  sync.Once
	stopped   chan struct{}
}

// NewReporter binds a Reporter to a Session. The Session retains ownership
// of its sockets — the Reporter only borrows them.
func NewReporter(s *Session, opts ReporterOptions) *Reporter {
	iv := opts.Interval
	if iv <= 0 {
		iv = DefaultRTCPInterval
	}
	cname := opts.CNAME
	if cname == "" {
		cname = defaultCNAME(s)
	}
	return &Reporter{
		sess:     s,
		interval: iv,
		cname:    cname,
		stopped:  make(chan struct{}),
	}
}

// Start launches the tick and inbound-RTCP goroutines. Idempotent — only the
// first call has effect. The goroutines exit when ctx is cancelled or the
// underlying Session closes.
func (r *Reporter) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		go r.tickLoop(ctx)
		go r.readLoop(ctx)
	})
}

// Stop signals the loops to exit and waits for them to acknowledge. It does
// not close the Session's sockets — Session.Close owns that.
func (r *Reporter) Stop() {
	r.stopOnce.Do(func() { close(r.stopped) })
}

func (r *Reporter) tickLoop(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.sess.closed:
			return
		case <-r.stopped:
			return
		case <-t.C:
			if err := r.emit(); err != nil {
				// Reporter errors are non-fatal — log and keep going. A
				// transient send failure (e.g. ICMP unreachable) should
				// not tear down the call.
				r.sess.logger.Debug("rtcp emit failed", "err", err.Error())
			}
		}
	}
}

func (r *Reporter) readLoop(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.sess.closed:
			return
		case <-r.stopped:
			return
		default:
		}
		// A short read deadline lets us notice cancellation without
		// blocking forever on a quiet peer.
		_ = r.sess.rtcpConn.SetReadDeadline(time.Now().Add(r.interval))
		n, _, err := r.sess.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			// Closed socket — exit.
			return
		}
		pkts, perr := rtcp.Unmarshal(buf[:n])
		if perr != nil {
			r.sess.logger.Debug("rtcp parse failed", "err", perr.Error(), "bytes", n)
			continue
		}
		for _, pkt := range pkts {
			if sr, ok := pkt.(*rtcp.SenderReport); ok {
				// Middle 32 bits of the 64-bit NTP timestamp — RFC 3550
				// §4 layout: hi 32 = seconds, lo 32 = fraction. The
				// reflected field is the middle 32 (bits 16..47).
				mid := uint32((sr.NTPTime >> 16) & 0xFFFFFFFF)
				r.sess.noteRemoteSR(mid, r.sess.now())
			}
		}
	}
}

// emit builds and sends a single compound RTCP packet appropriate for the
// session's current state. Returns nil if the session is idle (nothing to
// report yet) — that's a no-op, not an error.
func (r *Reporter) emit() error {
	dst := r.rtcpDest()
	if dst == nil {
		// Remote not set yet — pre-INVITE / pre-200 OK. Skip.
		return nil
	}

	txPackets := r.sess.txPackets.Load()
	txBytes := r.sess.txBytes.Load()
	rxSnap, hasRx := r.sess.rxSnap()

	if txPackets == 0 && !hasRx {
		// Session is silent in both directions — nothing to report.
		return nil
	}

	now := r.sess.now()
	var packets []rtcp.Packet
	if txPackets > 0 {
		packets = append(packets, r.buildSR(now, txPackets, txBytes, rxSnap, hasRx))
	} else {
		packets = append(packets, r.buildRR(rxSnap, hasRx, now))
	}
	packets = append(packets, &rtcp.SourceDescription{
		Chunks: []rtcp.SourceDescriptionChunk{{
			Source: r.sess.localSSRC,
			Items: []rtcp.SourceDescriptionItem{{
				Type: rtcp.SDESCNAME,
				Text: r.cname,
			}},
		}},
	})

	wire, err := rtcp.Marshal(packets)
	if err != nil {
		return err
	}
	_, err = r.sess.rtcpConn.WriteToUDP(wire, dst)
	return err
}

func (r *Reporter) buildSR(now time.Time, txPackets, txBytes uint64, rx rxSnapshot, hasRx bool) *rtcp.SenderReport {
	sr := &rtcp.SenderReport{
		SSRC:        r.sess.localSSRC,
		NTPTime:     ntpFromTime(now),
		RTPTime:     r.sess.lastTxTS.Load(),
		PacketCount: uint32(txPackets),
		OctetCount:  uint32(txBytes),
	}
	if hasRx {
		sr.Reports = []rtcp.ReceptionReport{r.buildReportBlock(rx, now)}
	}
	return sr
}

func (r *Reporter) buildRR(rx rxSnapshot, hasRx bool, now time.Time) *rtcp.ReceiverReport {
	rr := &rtcp.ReceiverReport{SSRC: r.sess.localSSRC}
	if hasRx {
		rr.Reports = []rtcp.ReceptionReport{r.buildReportBlock(rx, now)}
	}
	return rr
}

func (r *Reporter) buildReportBlock(rx rxSnapshot, now time.Time) rtcp.ReceptionReport {
	expected := rx.maxSeq - rx.baseSeq + 1
	var lost uint32
	if expected > rx.received {
		lost = expected - rx.received
	}
	// Fraction lost in this interval is approximated as cumulative ratio —
	// for a simple heartbeat this is good enough; the precise per-interval
	// computation can land alongside QoS surfaces in a later milestone.
	var fraction uint8
	if expected > 0 {
		fraction = uint8((uint64(lost) * 256) / uint64(expected))
	}

	var dlsr uint32
	if !rx.lastSRWhen.IsZero() {
		delta := now.Sub(rx.lastSRWhen).Seconds()
		if delta < 0 {
			delta = 0
		}
		// Units of 1/65536 second per RFC 3550 §6.4.1.
		dlsr = uint32(delta * 65536.0)
	}

	return rtcp.ReceptionReport{
		SSRC:               rx.ssrc,
		FractionLost:       fraction,
		TotalLost:          lost & 0x00FFFFFF, // 24-bit field
		LastSequenceNumber: rx.maxSeq,
		Jitter:             rx.jitter,
		LastSenderReport:   rx.lastSRMid,
		Delay:              dlsr,
	}
}

// rtcpDest computes where to send RTCP — the remote RTP port +1, per the
// RFC 3550 §11 even/odd convention. We don't yet honour an explicit SDP
// a=rtcp: attribute; that lands when SDP negotiation grows in a later slice.
func (r *Reporter) rtcpDest() *net.UDPAddr {
	rtpDst := r.sess.remote.Load()
	if rtpDst == nil {
		return nil
	}
	return &net.UDPAddr{IP: rtpDst.IP, Port: rtpDst.Port + 1, Zone: rtpDst.Zone}
}

// ntpFromTime converts a wall-clock time to a 64-bit NTP timestamp
// (seconds since 1900-01-01 in the high 32 bits, fractional seconds in the
// low 32 bits).
func ntpFromTime(t time.Time) uint64 {
	// Seconds between 1900-01-01 and 1970-01-01.
	const ntpEpochOffset = 2208988800
	sec := uint64(t.Unix() + ntpEpochOffset)
	// time.Time.Nanosecond is in [0, 1e9). Convert to 32-bit fraction.
	frac := uint64(t.Nanosecond()) * (1 << 32) / 1_000_000_000
	return (sec << 32) | frac
}

func defaultCNAME(s *Session) string {
	addr, ok := s.rtpConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "bellerophon"
	}
	return "bellerophon@" + addr.String()
}
