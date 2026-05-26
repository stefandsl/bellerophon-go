package rtp

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

func quietLogger() bellog.Logger { return bellog.New("error", "text") }

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		in        string
		wantMin   int
		wantMax   int
		wantError bool
	}{
		{"", 30000, 30100, false},
		{"30000-30100", 30000, 30100, false},
		{" 40000 - 40050 ", 40000, 40050, false},
		{"30001-30100", 30002, 30100, false}, // odd min bumped
		{"30000", 0, 0, true},
		{"30100-30000", 0, 0, true},
		{"500-30000", 0, 0, true},
		{"abc-def", 0, 0, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			pr, err := ParsePortRange(tc.in)
			if tc.wantError {
				if err == nil {
					t.Fatalf("want error, got %+v", pr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pr.Min != tc.wantMin || pr.Max != tc.wantMax {
				t.Fatalf("got %+v want {%d %d}", pr, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestSessionRoundTripG711Frame(t *testing.T) {
	// Receiver side.
	rx, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 41000, Max: 41100},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession rx: %v", err)
	}
	defer rx.Close()

	// Sender side, on a different port pair.
	tx, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 41200, Max: 41300},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession tx: %v", err)
	}
	defer tx.Close()

	if err := tx.SetRemote("127.0.0.1", rx.LocalPort()); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	// Send a synthetic G.711 frame (160 bytes = 20 ms @ 8 kHz).
	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = byte(i)
	}
	hdr := rtp.Header{
		Version:        2,
		PayloadType:    0, // PCMU
		SequenceNumber: 1234,
		Timestamp:      99999,
		SSRC:           0xdeadbeef,
		Marker:         true,
	}
	if err := tx.Send(hdr, payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case pkt := <-rx.Recv():
		if pkt.Header.SequenceNumber != 1234 {
			t.Errorf("seq: got %d want 1234", pkt.Header.SequenceNumber)
		}
		if pkt.Header.Timestamp != 99999 {
			t.Errorf("ts: got %d want 99999", pkt.Header.Timestamp)
		}
		if pkt.Header.SSRC != 0xdeadbeef {
			t.Errorf("ssrc: got %x want deadbeef", pkt.Header.SSRC)
		}
		if !pkt.Header.Marker {
			t.Error("marker bit lost")
		}
		if pkt.Header.PayloadType != 0 {
			t.Errorf("pt: got %d want 0", pkt.Header.PayloadType)
		}
		if len(pkt.Payload) != 160 {
			t.Fatalf("payload len: got %d want 160", len(pkt.Payload))
		}
		for i, b := range pkt.Payload {
			if b != byte(i) {
				t.Fatalf("payload[%d]=%d want %d", i, b, byte(i))
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for RTP packet on loopback")
	}

	if got := tx.Stats().TxPackets; got != 1 {
		t.Errorf("tx packets: got %d want 1", got)
	}
	if got := rx.Stats().RxPackets; got != 1 {
		t.Errorf("rx packets: got %d want 1", got)
	}
}

func TestSessionSendBeforeSetRemote(t *testing.T) {
	s, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 41400, Max: 41500},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	err = s.Send(rtp.Header{Version: 2}, []byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected error sending without SetRemote")
	}
}

func TestSessionDropsMalformedPacket(t *testing.T) {
	rx, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 41600, Max: 41700},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer rx.Close()

	// Raw UDP write of garbage that cannot parse as RTP.
	raw, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: rx.LocalPort(),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Write([]byte{0x00}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Give the read loop a moment to process.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rx.Stats().ParseErrors > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := rx.Stats().ParseErrors; got == 0 {
		t.Error("expected ParseErrors >= 1 on garbage input")
	}
	if got := rx.Stats().RxPackets; got != 0 {
		t.Errorf("RxPackets: got %d want 0 (parse failure should not bump rx)", got)
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	s, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 41800, Max: 41900},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	select {
	case _, ok := <-s.Recv():
		if ok {
			t.Error("Recv channel returned a value after close")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Recv channel not closed after Session.Close")
	}
}

func TestObserveRxJitterIsNonZeroOnVariableArrival(t *testing.T) {
	s := &Session{
		clockRate: 8000,
		now:       time.Now,
	}
	base := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return base }
	s.observeRx(0xAB, 1, 0)
	s.now = func() time.Time { return base.Add(20 * time.Millisecond) }
	s.observeRx(0xAB, 2, 160)
	if got := s.rxJitter; got > 1 {
		t.Errorf("jitter after on-time arrivals = %.3f, want ~0", got)
	}
	s.now = func() time.Time { return base.Add(50 * time.Millisecond) }
	s.observeRx(0xAB, 3, 320)
	if s.rxJitter <= 0 {
		t.Errorf("jitter after a late packet = %.3f, want > 0", s.rxJitter)
	}
}

func TestRTCPPortIsRTPPortPlusOne(t *testing.T) {
	s, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 42000, Max: 42100},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	if s.RTCPPort() != s.LocalPort()+1 {
		t.Errorf("RTCPPort=%d LocalPort=%d", s.RTCPPort(), s.LocalPort())
	}
}
