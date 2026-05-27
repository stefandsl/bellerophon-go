package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtp"

	"github.com/stefandsl/bellerophon-go/internal/config"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
	bellrtp "github.com/stefandsl/bellerophon-go/internal/rtp"
	"github.com/stefandsl/bellerophon-go/internal/rtp/rtcp"
	"github.com/stefandsl/bellerophon-go/internal/sipua"
)

// echoDelay is the audible delay the echo demo introduces. M001-SPEC §5 S04
// fixes 500 ms ± tolerance.
const echoDelay = 500 * time.Millisecond

// echoPreferredCodecs is the codec preference order we offer in the SDP answer.
// PCMU first matches what 3CX accepts today (M001-SPEC §7 risk-1 mitigation).
var echoPreferredCodecs = []uint8{sipua.PayloadPCMU, sipua.PayloadPCMA}

// echoHandler returns an InviteHandler that answers with a real SDP, opens an
// RTP session, and echoes inbound audio back to the caller with echoDelay
// latency. Tears the session down on BYE.
func echoHandler(cfg config.Config, logger bellog.Logger) func(*sipua.Call) {
	return func(c *sipua.Call) {
		callLog := logger.With("call_id", c.CallID, "mode", "echo")

		pr, err := bellrtp.ParsePortRange(cfg.RTP.PortRange)
		if err != nil {
			callLog.Error("rtp port range invalid", "error", err.Error())
			_ = c.Reply(500, "Server Internal Error", nil)
			return
		}
		sess, err := bellrtp.NewSession(bellrtp.Options{
			LocalIP:   "0.0.0.0",
			PortRange: pr,
			Logger:    callLog,
		})
		if err != nil {
			callLog.Error("rtp session open failed", "error", err.Error())
			_ = c.Reply(500, "Server Internal Error", nil)
			return
		}

		answer, pt, parsed, err := sipua.NegotiateAnswer(
			c.RemoteSDP,
			advertisedIPOrLocal(cfg.RTP.ExternalIP, sess),
			sess.LocalPort(),
			echoPreferredCodecs,
		)
		if err != nil {
			callLog.Error("sdp negotiation failed", "error", err.Error())
			_ = sess.Close()
			_ = c.Reply(488, "Not Acceptable Here", nil)
			return
		}

		remoteHost, remotePort, err := splitHostPort(parsed.RemoteAudioAddr())
		if err != nil {
			callLog.Error("remote sdp addr invalid", "error", err.Error())
			_ = sess.Close()
			_ = c.Reply(488, "Not Acceptable Here", nil)
			return
		}
		if err := sess.SetRemote(remoteHost, remotePort); err != nil {
			callLog.Error("rtp set remote failed", "error", err.Error())
			_ = sess.Close()
			_ = c.Reply(500, "Server Internal Error", nil)
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		reporter := rtcp.NewReporter(sess, rtcp.ReporterOptions{Logger: callLog})
		reporter.Start(ctx)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			runEchoLoop(ctx, echoLoopParams{
				In:          sess.Recv(),
				Send:        sess.Send,
				Delay:       echoDelay,
				PayloadType: pt,
				SSRC:        sess.LocalSSRC(),
				TSIncrement: 160, // 20 ms @ 8 kHz
				Now:         time.Now,
				Sleep:       sleepUntil,
				Logger:      callLog,
			})
		}()

		c.OnBye(func() {
			callLog.Info("bye received, tearing down echo session")
			cancel()
			reporter.Stop()
			_ = sess.Close()
			wg.Wait()
		})

		if err := c.Reply(200, "OK", answer); err != nil {
			callLog.Error("reply 200 failed", "error", err.Error())
			cancel()
			reporter.Stop()
			_ = sess.Close()
			wg.Wait()
			return
		}
		callLog.Info("echo session ready",
			"local_port", sess.LocalPort(),
			"remote", parsed.RemoteAudioAddr(),
			"codec_pt", pt,
		)
	}
}

// echoLoopParams collects the dependencies of runEchoLoop so the function is
// directly testable with synthetic inputs and an injected clock.
type echoLoopParams struct {
	In          <-chan bellrtp.Packet
	Send        func(rtp.Header, []byte) error
	Delay       time.Duration
	PayloadType uint8
	SSRC        uint32
	TSIncrement uint32
	Now         func() time.Time
	Sleep       func(context.Context, time.Time, func() time.Time)
	Logger      bellog.Logger
}

// runEchoLoop reads packets from p.In, waits p.Delay past their arrival time,
// then sends each one back through p.Send with a fresh RTP header (own SSRC,
// monotonic sequence and timestamp). Returns when p.In closes or ctx ends.
func runEchoLoop(ctx context.Context, p echoLoopParams) {
	type delayed struct {
		sendAt  time.Time
		payload []byte
	}
	// Buffer = ceil(delay / 20ms) + slack so the producer doesn't block on a
	// brief sender stall. 64 covers ~1.28 s, comfortable for the 500 ms target.
	queue := make(chan delayed, 64)

	go func() {
		defer close(queue)
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-p.In:
				if !ok {
					return
				}
				payload := make([]byte, len(pkt.Payload))
				copy(payload, pkt.Payload)
				select {
				case queue <- delayed{sendAt: p.Now().Add(p.Delay), payload: payload}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	var (
		seq uint16
		ts  uint32
	)
	for d := range queue {
		select {
		case <-ctx.Done():
			return
		default:
		}
		p.Sleep(ctx, d.sendAt, p.Now)
		select {
		case <-ctx.Done():
			return
		default:
		}
		h := rtp.Header{
			Version:        2,
			PayloadType:    p.PayloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           p.SSRC,
		}
		if err := p.Send(h, d.payload); err != nil {
			if p.Logger != nil {
				p.Logger.Debug("echo send failed", "error", err.Error())
			}
		}
		seq++
		ts += p.TSIncrement
	}
}

// sleepUntil sleeps until target according to now, observing ctx cancellation.
// Extracted so tests can substitute a deterministic implementation.
func sleepUntil(ctx context.Context, target time.Time, now func() time.Time) {
	d := target.Sub(now())
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// advertisedIPOrLocal picks the IP we put in the SDP c= line: the configured
// external IP when set, otherwise the local socket address. This mirrors what
// the stub SDP did before T05.
func advertisedIPOrLocal(externalIP string, sess *bellrtp.Session) string {
	if externalIP != "" {
		return externalIP
	}
	if sess == nil {
		return "127.0.0.1"
	}
	if la, ok := sess.LocalAddr().(*net.UDPAddr); ok && la.IP != nil && !la.IP.IsUnspecified() {
		return la.IP.String()
	}
	return "127.0.0.1"
}

// splitHostPort splits the "host:port" form produced by SDP.RemoteAudioAddr.
// SDP IP literals are usually bare IPv4 (no brackets); net.SplitHostPort
// handles that correctly.
func splitHostPort(addr string) (string, int, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("split %q: %w", addr, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("port %q out of range", p)
	}
	return h, port, nil
}
