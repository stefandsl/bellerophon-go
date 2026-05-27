package rtcp_test

import (
	"context"
	"net"
	"testing"
	"time"

	pionrtcp "github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
	"github.com/stefandsl/bellerophon-go/internal/rtp"
	"github.com/stefandsl/bellerophon-go/internal/rtp/rtcp"
)

func quietLogger() bellog.Logger { return bellog.New("error", "text") }

func TestReporterSendsSRAfterTx(t *testing.T) {
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

	s, err := rtp.NewSession(rtp.Options{
		LocalIP:   "127.0.0.1",
		PortRange: rtp.PortRange{Min: 43000, Max: 43100},
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

	if err := s.Send(pionrtp.Header{Version: 2, SSRC: 0xCAFEBABE, SequenceNumber: 1, Timestamp: 160}, make([]byte, 160)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	r := rtcp.NewReporter(s, rtcp.ReporterOptions{Interval: 30 * time.Millisecond, Logger: quietLogger()})
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
	pkts, err := pionrtcp.Unmarshal(buf[:n])
	if err != nil {
		t.Fatalf("rtcp parse: %v", err)
	}
	var sawSR, sawSDES bool
	for _, p := range pkts {
		switch v := p.(type) {
		case *pionrtcp.SenderReport:
			sawSR = true
			if v.SSRC != 0xCAFEBABE {
				t.Errorf("SR SSRC = %x want CAFEBABE", v.SSRC)
			}
			if v.PacketCount != 1 {
				t.Errorf("SR PacketCount = %d want 1", v.PacketCount)
			}
		case *pionrtcp.SourceDescription:
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

	s, err := rtp.NewSession(rtp.Options{
		LocalIP:   "127.0.0.1",
		PortRange: rtp.PortRange{Min: 43200, Max: 43300},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	if err := s.SetRemote("127.0.0.1", peerRTPPort); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	dialer, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: s.LocalPort()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer dialer.Close()
	pkt := pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, SSRC: 0x11223344, SequenceNumber: 7, Timestamp: 800},
		Payload: make([]byte, 160),
	}
	wire, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := dialer.Write(wire); err != nil {
		t.Fatalf("write: %v", err)
	}
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

	r := rtcp.NewReporter(s, rtcp.ReporterOptions{Interval: 30 * time.Millisecond, Logger: quietLogger()})
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
	pkts, err := pionrtcp.Unmarshal(buf[:n])
	if err != nil {
		t.Fatalf("rtcp parse: %v", err)
	}
	var rr *pionrtcp.ReceiverReport
	for _, p := range pkts {
		if v, ok := p.(*pionrtcp.ReceiverReport); ok {
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

	s, err := rtp.NewSession(rtp.Options{
		LocalIP:   "127.0.0.1",
		PortRange: rtp.PortRange{Min: 43400, Max: 43500},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	if err := s.SetRemote("127.0.0.1", peerRTPPort); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	r := rtcp.NewReporter(s, rtcp.ReporterOptions{Interval: 20 * time.Millisecond, Logger: quietLogger()})
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
