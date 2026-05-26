package rtp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// TestReporterSendsSRAfterTx verifies that once the session has sent at least
// one RTP packet, the Reporter emits a compound RTCP packet containing a
// SenderReport on the next tick.
func TestReporterSendsSRAfterTx(t *testing.T) {
	// Set up the peer's RTCP socket first so we know which port to use as
	// the remote — the Reporter targets remote_rtp_port+1.
	peerRTCP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen peer rtcp: %v", err)
	}
	defer peerRTCP.Close()
	peerRTCPPort := peerRTCP.LocalAddr().(*net.UDPAddr).Port
	peerRTPPort := peerRTCPPort - 1
	if peerRTPPort%2 != 0 {
		t.Skipf("randomly allocated port %d is odd; rerun", peerRTCPPort)
	}

	s, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 43000, Max: 43100},
		Logger:    quietLogger(),
		SSRC:      0xCAFEBABE,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if err := s.SetRemote("127.0.0.1", peerRTPPort); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	// Send one RTP packet so the Reporter has something to report.
	if err := s.Send(rtp.Header{Version: 2, SSRC: 0xCAFEBABE, SequenceNumber: 1, Timestamp: 160}, make([]byte, 160)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	r := NewReporter(s, ReporterOptions{Interval: 30 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Stop()

	_ = peerRTCP.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := peerRTCP.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	pkts, err := rtcp.Unmarshal(buf[:n])
	if err != nil {
		t.Fatalf("rtcp parse: %v", err)
	}
	var sawSR, sawSDES bool
	for _, p := range pkts {
		switch v := p.(type) {
		case *rtcp.SenderReport:
			sawSR = true
			if v.SSRC != 0xCAFEBABE {
				t.Errorf("SR SSRC = %x want CAFEBABE", v.SSRC)
			}
			if v.PacketCount != 1 {
				t.Errorf("SR PacketCount = %d want 1", v.PacketCount)
			}
		case *rtcp.SourceDescription:
			sawSDES = true
		}
	}
	if !sawSR {
		t.Error("compound RTCP did not contain a SenderReport")
	}
	if !sawSDES {
		t.Error("compound RTCP did not contain SDES (required by RFC 3550 §6.3.9)")
	}
}

// TestReporterSendsRRWhenOnlyReceiving verifies that a receive-only session
// (no Send calls) emits a ReceiverReport once it has observed inbound RTP.
func TestReporterSendsRRWhenOnlyReceiving(t *testing.T) {
	peerRTCP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen peer rtcp: %v", err)
	}
	defer peerRTCP.Close()
	peerRTCPPort := peerRTCP.LocalAddr().(*net.UDPAddr).Port
	peerRTPPort := peerRTCPPort - 1
	if peerRTPPort%2 != 0 {
		t.Skipf("randomly allocated port %d is odd; rerun", peerRTCPPort)
	}

	s, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 43200, Max: 43300},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	if err := s.SetRemote("127.0.0.1", peerRTPPort); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	// Inject one inbound RTP packet directly to the session's RTP port.
	dialer, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: s.LocalPort()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer dialer.Close()
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, SSRC: 0x11223344, SequenceNumber: 7, Timestamp: 800},
		Payload: make([]byte, 160),
	}
	wire, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := dialer.Write(wire); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Wait for the session to observe the packet.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.Stats().RxPackets > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.Stats().RxPackets == 0 {
		t.Fatal("session never observed the injected RTP packet")
	}

	r := NewReporter(s, ReporterOptions{Interval: 30 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Stop()

	_ = peerRTCP.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := peerRTCP.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	pkts, err := rtcp.Unmarshal(buf[:n])
	if err != nil {
		t.Fatalf("rtcp parse: %v", err)
	}
	var rr *rtcp.ReceiverReport
	for _, p := range pkts {
		if v, ok := p.(*rtcp.ReceiverReport); ok {
			rr = v
		}
	}
	if rr == nil {
		t.Fatal("expected ReceiverReport in compound RTCP")
	}
	if len(rr.Reports) != 1 {
		t.Fatalf("rr blocks = %d want 1", len(rr.Reports))
	}
	if rr.Reports[0].SSRC != 0x11223344 {
		t.Errorf("RR block SSRC = %x want 11223344", rr.Reports[0].SSRC)
	}
}

// TestReporterSilentWhenIdle ensures the reporter does not send anything
// until the session has either sent or received an RTP packet.
func TestReporterSilentWhenIdle(t *testing.T) {
	peerRTCP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen peer rtcp: %v", err)
	}
	defer peerRTCP.Close()
	peerRTCPPort := peerRTCP.LocalAddr().(*net.UDPAddr).Port
	peerRTPPort := peerRTCPPort - 1
	if peerRTPPort%2 != 0 {
		t.Skipf("odd peer port %d", peerRTCPPort)
	}

	s, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 43400, Max: 43500},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	if err := s.SetRemote("127.0.0.1", peerRTPPort); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	r := NewReporter(s, ReporterOptions{Interval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Stop()

	_ = peerRTCP.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 1500)
	if _, _, err := peerRTCP.ReadFromUDP(buf); err == nil {
		t.Fatal("idle session emitted RTCP — should stay silent until first RTP")
	}
}

// TestObserveRxJitterIsNonZeroOnVariableArrival sanity-checks the jitter
// accumulator: identical inter-arrival intervals yield ~zero jitter, while a
// gap produces a measurable positive value.
func TestObserveRxJitterIsNonZeroOnVariableArrival(t *testing.T) {
	s := &Session{
		clockRate: 8000,
		now:       time.Now,
	}
	// Burn first packet to anchor transit. Use synthetic times.
	base := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return base }
	s.observeRx(0xAB, 1, 0)
	s.now = func() time.Time { return base.Add(20 * time.Millisecond) }
	s.observeRx(0xAB, 2, 160) // perfectly on-time
	if got := s.rxJitter; got > 1 {
		t.Errorf("jitter after on-time arrivals = %.3f, want ~0", got)
	}
	// Now a packet that arrives 10 ms late — RTP ts advances by 160 (20ms)
	// but wall clock advances by 30 ms.
	s.now = func() time.Time { return base.Add(50 * time.Millisecond) }
	s.observeRx(0xAB, 3, 320)
	if s.rxJitter <= 0 {
		t.Errorf("jitter after a late packet = %.3f, want > 0", s.rxJitter)
	}
}
