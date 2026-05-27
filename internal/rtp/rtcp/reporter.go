package rtcp

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pion/rtcp"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
	"github.com/stefandsl/bellerophon-go/internal/rtp"
)

const DefaultRTCPInterval = 5 * time.Second

type ReporterOptions struct {
	Interval time.Duration
	CNAME    string
	Logger   bellog.Logger
}

type Reporter struct {
	sess     *rtp.Session
	logger   bellog.Logger
	interval time.Duration
	cname    string

	startOnce sync.Once
	stopOnce  sync.Once
	stopped   chan struct{}
}

func NewReporter(s *rtp.Session, opts ReporterOptions) *Reporter {
	iv := opts.Interval
	if iv <= 0 {
		iv = DefaultRTCPInterval
	}
	cname := opts.CNAME
	if cname == "" {
		cname = defaultCNAME(s)
	}
	lg := opts.Logger
	if lg == nil {
		lg = bellog.New("error", "text")
	}
	return &Reporter{
		sess:     s,
		logger:   lg,
		interval: iv,
		cname:    cname,
		stopped:  make(chan struct{}),
	}
}

func (r *Reporter) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		go r.tickLoop(ctx)
		go r.readLoop(ctx)
	})
}

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
		case <-r.sess.Done():
			return
		case <-r.stopped:
			return
		case <-t.C:
			if err := r.emit(); err != nil {
				r.logger.Debug("rtcp emit failed", "err", err.Error())
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
		case <-r.sess.Done():
			return
		case <-r.stopped:
			return
		default:
		}
		n, _, err := r.sess.ReadRTCP(buf, time.Now().Add(r.interval))
		if err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			return
		}
		pkts, perr := rtcp.Unmarshal(buf[:n])
		if perr != nil {
			r.logger.Debug("rtcp parse failed", "err", perr.Error(), "bytes", n)
			continue
		}
		for _, pkt := range pkts {
			if sr, ok := pkt.(*rtcp.SenderReport); ok {
				mid := uint32((sr.NTPTime >> 16) & 0xFFFFFFFF) //nolint:gosec // masked to 32 bits, fits by construction
				r.sess.NoteRemoteSR(mid, r.sess.Now())
			}
		}
	}
}

func (r *Reporter) emit() error {
	dst := r.rtcpDest()
	if dst == nil {
		return nil
	}

	st := r.sess.Stats()
	txPackets := st.TxPackets
	txBytes := st.TxBytes
	rxSnap, hasRx := r.sess.RxSnapshot()

	if txPackets == 0 && !hasRx {
		return nil
	}

	now := r.sess.Now()
	var packets []rtcp.Packet
	if txPackets > 0 {
		packets = append(packets, r.buildSR(now, txPackets, txBytes, rxSnap, hasRx))
	} else {
		packets = append(packets, r.buildRR(rxSnap, hasRx, now))
	}
	packets = append(packets, &rtcp.SourceDescription{
		Chunks: []rtcp.SourceDescriptionChunk{{
			Source: r.sess.LocalSSRC(),
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
	_, err = r.sess.WriteRTCP(wire, dst)
	return err
}

func (r *Reporter) buildSR(now time.Time, txPackets, txBytes uint64, rx rtp.RxSnapshot, hasRx bool) *rtcp.SenderReport {
	sr := &rtcp.SenderReport{
		SSRC:        r.sess.LocalSSRC(),
		NTPTime:     ntpFromTime(now),
		RTPTime:     r.sess.LastTxTimestamp(),
		PacketCount: uint32(txPackets & 0xFFFFFFFF), //nolint:gosec // RFC 3550 expects modular wrap at 2^32
		OctetCount:  uint32(txBytes & 0xFFFFFFFF),   //nolint:gosec // RFC 3550 expects modular wrap at 2^32
	}
	if hasRx {
		sr.Reports = []rtcp.ReceptionReport{r.buildReportBlock(rx, now)}
	}
	return sr
}

func (r *Reporter) buildRR(rx rtp.RxSnapshot, hasRx bool, now time.Time) *rtcp.ReceiverReport {
	rr := &rtcp.ReceiverReport{SSRC: r.sess.LocalSSRC()}
	if hasRx {
		rr.Reports = []rtcp.ReceptionReport{r.buildReportBlock(rx, now)}
	}
	return rr
}

func (r *Reporter) buildReportBlock(rx rtp.RxSnapshot, now time.Time) rtcp.ReceptionReport {
	expected := rx.MaxSeq - rx.BaseSeq + 1
	var lost uint32
	if expected > rx.Received {
		lost = expected - rx.Received
	}
	var fraction uint8
	if expected > 0 {
		fraction = uint8((uint64(lost) * 256) / uint64(expected)) //nolint:gosec // lost ≤ expected, so quotient ≤ 256; uint8 sufficient
	}

	var dlsr uint32
	if !rx.LastSRWhen.IsZero() {
		delta := now.Sub(rx.LastSRWhen).Seconds()
		if delta < 0 {
			delta = 0
		}
		dlsr = uint32(delta * 65536.0)
	}

	return rtcp.ReceptionReport{
		SSRC:               rx.SSRC,
		FractionLost:       fraction,
		TotalLost:          lost & 0x00FFFFFF,
		LastSequenceNumber: rx.MaxSeq,
		Jitter:             rx.Jitter,
		LastSenderReport:   rx.LastSRMid,
		Delay:              dlsr,
	}
}

func (r *Reporter) rtcpDest() *net.UDPAddr {
	rtpDst := r.sess.Remote()
	if rtpDst == nil {
		return nil
	}
	return &net.UDPAddr{IP: rtpDst.IP, Port: rtpDst.Port + 1, Zone: rtpDst.Zone}
}

func ntpFromTime(t time.Time) uint64 {
	const ntpEpochOffset = 2208988800
	sec := uint64(t.Unix() + ntpEpochOffset)                   //nolint:gosec // post-1900 time always yields a positive int64
	frac := uint64(t.Nanosecond()) * (1 << 32) / 1_000_000_000 //nolint:gosec // Nanosecond() returns [0, 1e9-1]
	return (sec << 32) | frac
}

func defaultCNAME(s *rtp.Session) string {
	addr, ok := s.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "bellerophon"
	}
	return "bellerophon@" + addr.String()
}
