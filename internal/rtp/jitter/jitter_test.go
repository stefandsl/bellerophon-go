package jitter

import (
	"testing"
	"time"

	"github.com/pion/rtp"

	irtp "github.com/stefandsl/bellerophon-go/internal/rtp"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func mkPkt(seq uint16, ts uint32) irtp.Packet {
	return irtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, Timestamp: ts, PayloadType: 0},
		Payload: []byte{byte(seq)},
	}
}

func newTestJB(clk *fakeClock) *JitterBuffer {
	return NewJitterBuffer(JBOptions{
		TargetDelay: 60 * time.Millisecond,
		MaxLate:     100 * time.Millisecond,
		Ptime:       20 * time.Millisecond,
		Capacity:    8,
		Now:         clk.now,
	})
}

func TestJitterBuffer_InOrderPushPop(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	jb := newTestJB(clk)

	for i := uint16(0); i < 3; i++ {
		if !jb.Push(mkPkt(100+i, 8000+uint32(i)*160)) {
			t.Fatalf("Push %d rejected", i)
		}
		clk.advance(20 * time.Millisecond)
	}

	pkt, ok := jb.Pop()
	if !ok {
		t.Fatalf("Pop at scheduled time: expected ok")
	}
	if pkt.Header.SequenceNumber != 100 {
		t.Fatalf("first popped seq = %d, want 100", pkt.Header.SequenceNumber)
	}

	if _, ok := jb.Pop(); ok {
		t.Fatalf("Pop before packet 1 due: expected false")
	}
	clk.advance(20 * time.Millisecond)
	pkt, ok = jb.Pop()
	if !ok || pkt.Header.SequenceNumber != 101 {
		t.Fatalf("packet 1: ok=%v seq=%d", ok, pkt.Header.SequenceNumber)
	}
	clk.advance(20 * time.Millisecond)
	pkt, ok = jb.Pop()
	if !ok || pkt.Header.SequenceNumber != 102 {
		t.Fatalf("packet 2: ok=%v seq=%d", ok, pkt.Header.SequenceNumber)
	}

	if s := jb.Stats(); s.Popped != 3 || s.DroppedLate != 0 || s.DroppedExpired != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestJitterBuffer_ReordersPackets(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	jb := newTestJB(clk)

	for _, seq := range []uint16{100, 102, 101} {
		if !jb.Push(mkPkt(seq, 8000+uint32(seq-100)*160)) {
			t.Fatalf("Push %d rejected", seq)
		}
		clk.advance(10 * time.Millisecond)
	}

	clk.advance(30 * time.Millisecond)
	got := make([]uint16, 0, 3)
	for {
		pkt, ok := jb.Pop()
		if !ok {
			break
		}
		got = append(got, pkt.Header.SequenceNumber)
	}
	if len(got) != 1 || got[0] != 100 {
		t.Fatalf("first batch: got %v, want [100]", got)
	}

	clk.advance(20 * time.Millisecond)
	pkt, ok := jb.Pop()
	if !ok || pkt.Header.SequenceNumber != 101 {
		t.Fatalf("expected 101, got ok=%v seq=%d", ok, pkt.Header.SequenceNumber)
	}
	clk.advance(20 * time.Millisecond)
	pkt, ok = jb.Pop()
	if !ok || pkt.Header.SequenceNumber != 102 {
		t.Fatalf("expected 102, got ok=%v seq=%d", ok, pkt.Header.SequenceNumber)
	}
}

func TestJitterBuffer_DropsLateArrivals(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	jb := newTestJB(clk)

	if !jb.Push(mkPkt(100, 8000)) {
		t.Fatalf("push 100")
	}
	clk.advance(60 * time.Millisecond)
	if _, ok := jb.Pop(); !ok {
		t.Fatalf("pop 100")
	}

	if jb.Push(mkPkt(99, 8000-160)) {
		t.Fatalf("Push(99) after Pop(100) should be rejected")
	}
	if s := jb.Stats(); s.DroppedLate != 1 {
		t.Fatalf("DroppedLate = %d, want 1", s.DroppedLate)
	}
}

func TestJitterBuffer_DropsExpiredHead(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	jb := newTestJB(clk)

	jb.Push(mkPkt(100, 8000))
	jb.Push(mkPkt(101, 8160))
	clk.advance(300 * time.Millisecond)

	if _, ok := jb.Pop(); ok {
		t.Fatalf("expected empty after both expired")
	}
	if s := jb.Stats(); s.DroppedExpired != 2 || s.Popped != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestJitterBuffer_SequenceWraparound(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	jb := newTestJB(clk)

	seqs := []uint16{65534, 65535, 0, 1}
	for _, s := range seqs {
		if !jb.Push(mkPkt(s, uint32(s)*160)) {
			t.Fatalf("push %d rejected", s)
		}
		clk.advance(5 * time.Millisecond)
	}

	clk.advance(200 * time.Millisecond)

	got := make([]uint16, 0, 4)
	for {
		pkt, ok := jb.Pop()
		if !ok {
			break
		}
		got = append(got, pkt.Header.SequenceNumber)
	}
	want := []uint16{65534, 65535, 0, 1}
	if !isInWrapOrder(got, want) {
		t.Fatalf("got %v; not a wrap-ordered subsequence of %v", got, want)
	}
}

func isInWrapOrder(got, want []uint16) bool {
	i := 0
	for _, w := range want {
		if i < len(got) && got[i] == w {
			i++
		}
	}
	return i == len(got)
}

func TestJitterBuffer_CapacityOverflowEvictsHead(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	jb := NewJitterBuffer(JBOptions{
		TargetDelay: 60 * time.Millisecond,
		MaxLate:     500 * time.Millisecond,
		Ptime:       20 * time.Millisecond,
		Capacity:    3,
		Now:         clk.now,
	})

	for i := uint16(0); i < 4; i++ {
		if !jb.Push(mkPkt(100+i, 8000+uint32(i)*160)) {
			t.Fatalf("push %d rejected", i)
		}
	}
	s := jb.Stats()
	if s.DroppedOverflow != 1 {
		t.Fatalf("DroppedOverflow = %d, want 1", s.DroppedOverflow)
	}
	if s.Depth != 3 {
		t.Fatalf("Depth = %d, want 3", s.Depth)
	}

	clk.advance(200 * time.Millisecond)
	got := make([]uint16, 0, 3)
	for {
		pkt, ok := jb.Pop()
		if !ok {
			break
		}
		got = append(got, pkt.Header.SequenceNumber)
	}
	if len(got) != 3 || got[0] != 101 || got[1] != 102 || got[2] != 103 {
		t.Fatalf("survivors = %v, want [101 102 103]", got)
	}
}

func TestJitterBuffer_TimestampJumpKeepsClockOnSequence(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	jb := newTestJB(clk)

	jb.Push(mkPkt(100, 8000))
	jb.Push(mkPkt(101, 8000+5*8000))

	clk.advance(60 * time.Millisecond)
	if pkt, ok := jb.Pop(); !ok || pkt.Header.SequenceNumber != 100 {
		t.Fatalf("first: ok=%v seq=%d", ok, pkt.Header.SequenceNumber)
	}
	clk.advance(20 * time.Millisecond)
	pkt, ok := jb.Pop()
	if !ok || pkt.Header.SequenceNumber != 101 {
		t.Fatalf("post-jump: ok=%v seq=%d", ok, pkt.Header.SequenceNumber)
	}
}

func TestJitterBuffer_DefaultsWhenZero(t *testing.T) {
	jb := NewJitterBuffer(JBOptions{})
	if jb.target != DefaultTargetDelay || jb.maxLate != DefaultMaxLate ||
		jb.ptime != DefaultPtime || jb.cap == 0 || jb.now == nil {
		t.Fatalf("defaults not applied: %+v", jb)
	}
}
