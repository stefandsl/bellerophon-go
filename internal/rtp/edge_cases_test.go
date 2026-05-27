package rtp

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

// TestObserveRx_SequenceWraparound feeds the 16-bit seq into observeRx across
// the 65535 → 0 boundary and asserts that rxCycles increments exactly once
// and rxMaxSeq extends correctly into the next cycle.
func TestObserveRx_SequenceWraparound(t *testing.T) {
	s := &Session{clockRate: 8000, now: time.Now}

	// First packet sets the base near the wrap point.
	s.observeRx(0xA1, 65530, 0)
	if s.rxCycles != 0 {
		t.Fatalf("cycles after first packet = %d, want 0", s.rxCycles)
	}

	// In-cycle progression up to 65535.
	for seq := uint16(65531); seq != 0; seq++ {
		s.observeRx(0xA1, seq, uint32(seq)*160)
		if s.rxCycles != 0 {
			t.Fatalf("cycles at seq=%d = %d, want 0", seq, s.rxCycles)
		}
	}

	// Wrap to 0 — must bump rxCycles and the extended max to 0x10000.
	s.observeRx(0xA1, 0, 0)
	if s.rxCycles != 1 {
		t.Fatalf("cycles after wrap = %d, want 1", s.rxCycles)
	}
	wantExt := uint32(1 << 16)
	if s.rxMaxSeq != wantExt {
		t.Fatalf("rxMaxSeq after wrap = %d, want %d", s.rxMaxSeq, wantExt)
	}

	// Continue into the next cycle.
	s.observeRx(0xA1, 1, 160)
	if s.rxCycles != 1 {
		t.Fatalf("cycles in next cycle = %d, want 1", s.rxCycles)
	}
	if s.rxMaxSeq != (1<<16)|1 {
		t.Fatalf("rxMaxSeq after one in next cycle = %d, want %d", s.rxMaxSeq, (1<<16)|1)
	}
}

// TestObserveRx_TimestampJumpCNG simulates silence suppression / comfort
// noise: the remote stops sending audio frames for ~1 s, then resumes with a
// large RTP timestamp jump. The jitter accumulator must keep producing finite
// non-negative values; no overflow, no NaN, no negative.
func TestObserveRx_TimestampJumpCNG(t *testing.T) {
	s := &Session{clockRate: 8000}
	base := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return base }

	// Three on-pace G.711 packets at 20 ms.
	for i := uint16(0); i < 3; i++ {
		s.now = func() time.Time { return base.Add(time.Duration(i) * 20 * time.Millisecond) }
		s.observeRx(0xBE, 100+i, uint32(i)*160)
	}
	preJitter := s.rxJitter

	// 1 s gap on the wire + a big timestamp jump (8 kHz * 1 s = 8000 ticks)
	// matching the gap, which is what well-behaved CNG generators do.
	s.now = func() time.Time { return base.Add(1020 * time.Millisecond) }
	s.observeRx(0xBE, 103, 3*160+8000)
	if s.rxJitter < 0 {
		t.Fatalf("jitter went negative on CNG resume: %f", s.rxJitter)
	}
	// Sanity: an aligned CNG resume should not blow jitter up dramatically.
	// The jitter EWMA dampens with 1/16, so even if a stray sample arrives
	// the running mean stays bounded.
	if s.rxJitter > 1e9 {
		t.Fatalf("jitter overflow on CNG resume: %f (was %f)", s.rxJitter, preJitter)
	}

	// A mis-aligned timestamp jump (peer's clock raced ahead of wire time) —
	// jitter should increase but stay finite.
	s.now = func() time.Time { return base.Add(1040 * time.Millisecond) }
	s.observeRx(0xBE, 104, 3*160+8000+160+99999)
	if s.rxJitter < 0 || s.rxJitter > 1e15 {
		t.Fatalf("jitter not finite after misaligned jump: %f", s.rxJitter)
	}
}

// TestSession_ToleratesSyntheticPacketLoss sends 200 RTP packets from a
// loopback peer, dropping every 20th (5 % loss). The receiver must:
//   - receive exactly the un-dropped count
//   - not raise ParseErrors
//   - keep observeRx state coherent (no panic, rxMaxSeq advances)
//
// Per M001-SPEC §5 S04: "drop 5 % of incoming packets — output must still be
// intelligible (no chained-error retransmits, no buffer underrun crashes)."
func TestSession_ToleratesSyntheticPacketLoss(t *testing.T) {
	rx, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 43000, Max: 43100},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("rx NewSession: %v", err)
	}
	defer rx.Close()

	tx, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 43200, Max: 43300},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("tx NewSession: %v", err)
	}
	defer tx.Close()
	if err := tx.SetRemote("127.0.0.1", rx.LocalPort()); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	const total = 200
	const lossEvery = 20 // 5 % loss

	payload := make([]byte, 160)
	for i := range payload {
		payload[i] = byte(i)
	}

	var sent, drained int
	go func() {
		for i := 0; i < total; i++ {
			if i%lossEvery == 0 {
				continue // "dropped on the wire"
			}
			hdr := rtp.Header{
				Version:        2,
				PayloadType:    0,
				SequenceNumber: uint16(1000 + i), //nolint:gosec // i bounded
				Timestamp:      uint32(160 * i),  //nolint:gosec // i bounded
				SSRC:           0x5055,
			}
			if err := tx.Send(hdr, payload); err != nil {
				t.Errorf("Send(%d): %v", i, err)
				return
			}
			sent++
			time.Sleep(200 * time.Microsecond) // pace gently to avoid UDP coalescing drops
		}
	}()

	deadline := time.After(2 * time.Second)
	wantReceived := total - total/lossEvery
	for drained < wantReceived {
		select {
		case <-rx.Recv():
			drained++
		case <-deadline:
			t.Fatalf("timed out: drained=%d want=%d (tx.sent=%d)", drained, wantReceived, sent)
		}
	}

	stats := rx.Stats()
	if stats.RxPackets != uint64(wantReceived) { //nolint:gosec // wantReceived ≥ 0
		t.Errorf("RxPackets=%d want %d", stats.RxPackets, wantReceived)
	}
	if stats.ParseErrors != 0 {
		t.Errorf("ParseErrors=%d want 0 (synthetic drops are wire-level, not parse)", stats.ParseErrors)
	}
	snap, ok := rx.RxSnapshot()
	if !ok {
		t.Fatal("RxSnapshot returned !ok after a flow")
	}
	if snap.MaxSeq < 1000+uint32(total)-2 {
		t.Errorf("MaxSeq=%d, want ≥ %d", snap.MaxSeq, 1000+uint32(total)-2)
	}
	// Reported "lost" should be close to the synthetic drop count. Allow ±1
	// because the first packet is at i=0 (dropped) so the base seq is 1001.
	expected := snap.MaxSeq - snap.BaseSeq + 1
	if expected < snap.Received {
		t.Fatalf("expected < received: %d < %d", expected, snap.Received)
	}
	lost := expected - snap.Received
	wantLost := uint32(total/lossEvery) - 1 // first slot dropped lives outside the base
	if lost+1 < wantLost || lost > wantLost+1 {
		t.Errorf("derived lost=%d, want ~%d (±1)", lost, wantLost)
	}
}

// TestSessionRoundTrip_PreservesMarkerBit is a dedicated marker-bit test
// (M001-SPEC §5 S04 must-have, called out separately even though
// TestSessionRoundTripG711Frame exercises it). Both Marker=true and
// Marker=false must survive the wire round trip without inversion.
func TestSessionRoundTrip_PreservesMarkerBit(t *testing.T) {
	rx, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 43400, Max: 43500},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("rx NewSession: %v", err)
	}
	defer rx.Close()
	tx, err := NewSession(Options{
		LocalIP:   "127.0.0.1",
		PortRange: PortRange{Min: 43600, Max: 43700},
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("tx NewSession: %v", err)
	}
	defer tx.Close()
	if err := tx.SetRemote("127.0.0.1", rx.LocalPort()); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	for i, marker := range []bool{false, true, false, true, false} {
		hdr := rtp.Header{
			Version:        2,
			PayloadType:    0,
			SequenceNumber: uint16(2000 + i), //nolint:gosec // i bounded
			Timestamp:      uint32(160 * i),  //nolint:gosec // i bounded
			SSRC:           0xABCDEF,
			Marker:         marker,
		}
		if err := tx.Send(hdr, []byte{1, 2, 3, 4}); err != nil {
			t.Fatalf("Send(%d): %v", i, err)
		}
		select {
		case pkt := <-rx.Recv():
			if pkt.Header.Marker != marker {
				t.Errorf("packet %d marker round-trip: got %v want %v", i, pkt.Header.Marker, marker)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("packet %d (marker=%v) not received", i, marker)
		}
	}
}

// TestSession_RemoteSSRCChangeReset ensures that when the peer SSRC changes
// mid-stream (interpreted as a fresh remote source per RFC 3550) the session
// resets its inbound bookkeeping cleanly instead of producing nonsensical
// extended-sequence math.
func TestSession_RemoteSSRCChangeReset(t *testing.T) {
	s := &Session{clockRate: 8000, now: time.Now}
	for seq := uint16(100); seq < 110; seq++ {
		s.observeRx(0xAAAA, seq, uint32(seq)*160)
	}
	if s.rxSSRC != 0xAAAA || s.rxRecv != 10 {
		t.Fatalf("pre-state: ssrc=%x rxRecv=%d", s.rxSSRC, s.rxRecv)
	}
	// Peer SSRC changes — fresh stream, all counters reset.
	s.observeRx(0xBBBB, 5, 0)
	if s.rxSSRC != 0xBBBB {
		t.Errorf("ssrc not switched: got %x want bbbb", s.rxSSRC)
	}
	if s.rxRecv != 1 {
		t.Errorf("rxRecv after SSRC switch = %d, want 1 (reset)", s.rxRecv)
	}
	if s.rxBaseSeq != 5 || s.rxMaxSeq != 5 {
		t.Errorf("base/max seq after SSRC switch = %d/%d, want 5/5",
			s.rxBaseSeq, s.rxMaxSeq)
	}
	if s.rxCycles != 0 {
		t.Errorf("rxCycles after SSRC switch = %d, want 0", s.rxCycles)
	}
}
