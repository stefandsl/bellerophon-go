package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
	bellrtp "github.com/stefandsl/bellerophon-go/internal/rtp"
)

// fakeClock is a mutex-protected clock used in echo-loop tests; the producer
// reads via Now() while the consumer advances it via Set().
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.After(f.t) {
		f.t = t
	}
}

func TestRunEchoLoop_PreservesPayloadAndAppliesDelay(t *testing.T) {
	t.Parallel()

	in := make(chan bellrtp.Packet, 4)
	type sentPkt struct {
		at   time.Time
		hdr  rtp.Header
		data []byte
	}

	var sentMu sync.Mutex
	var sent []sentPkt

	base := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}

	// Deterministic sleep: advance the clock to the target instead of blocking.
	sleep := func(_ context.Context, target time.Time, _ func() time.Time) {
		clock.Set(target)
	}
	send := func(h rtp.Header, payload []byte) error {
		sentMu.Lock()
		sent = append(sent, sentPkt{at: clock.Now(), hdr: h, data: append([]byte(nil), payload...)})
		sentMu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := echoLoopParams{
		In:          in,
		Send:        send,
		Delay:       500 * time.Millisecond,
		PayloadType: 0,
		SSRC:        0xCAFEBABE,
		TSIncrement: 160,
		Now:         clock.Now,
		Sleep:       sleep,
		Logger:      bellog.New("error", "text"),
	}

	done := make(chan struct{})
	go func() {
		runEchoLoop(ctx, p)
		close(done)
	}()

	payloads := [][]byte{
		{0x01, 0x02, 0x03, 0x04},
		{0x10, 0x20, 0x30, 0x40},
		{0xAA, 0xBB, 0xCC, 0xDD},
	}
	for _, pl := range payloads {
		in <- bellrtp.Packet{Header: rtp.Header{}, Payload: pl}
	}
	close(in)

	<-done

	sentMu.Lock()
	defer sentMu.Unlock()
	if len(sent) != len(payloads) {
		t.Fatalf("sent %d packets, want %d", len(sent), len(payloads))
	}
	for i, pl := range payloads {
		got := sent[i]
		if string(got.data) != string(pl) {
			t.Errorf("packet %d payload = %v, want %v", i, got.data, pl)
		}
		if got.hdr.SSRC != p.SSRC {
			t.Errorf("packet %d SSRC = %#x, want %#x", i, got.hdr.SSRC, p.SSRC)
		}
		if got.hdr.PayloadType != p.PayloadType {
			t.Errorf("packet %d PT = %d, want %d", i, got.hdr.PayloadType, p.PayloadType)
		}
		wantSeq := uint16(i) //nolint:gosec // i is small loop index
		if got.hdr.SequenceNumber != wantSeq {
			t.Errorf("packet %d seq = %d, want %d", i, got.hdr.SequenceNumber, wantSeq)
		}
		wantTS := uint32(i) * p.TSIncrement //nolint:gosec // i is small loop index
		if got.hdr.Timestamp != wantTS {
			t.Errorf("packet %d ts = %d, want %d", i, got.hdr.Timestamp, wantTS)
		}
		// The deterministic-sleep clock only advances forward, so every
		// packet's recorded sendAt is at least base + delay.
		if got.at.Before(base.Add(p.Delay)) {
			t.Errorf("packet %d sentAt = %v, want ≥ %v", i, got.at, base.Add(p.Delay))
		}
	}
}

func TestRunEchoLoop_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	in := make(chan bellrtp.Packet)
	send := func(rtp.Header, []byte) error { return nil }
	clock := time.Now()
	now := func() time.Time { return clock }
	sleep := func(ctx context.Context, _ time.Time, _ func() time.Time) {
		<-ctx.Done() // Block until cancelled.
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runEchoLoop(ctx, echoLoopParams{
			In: in, Send: send, Delay: 100 * time.Millisecond,
			PayloadType: 0, SSRC: 1, TSIncrement: 160,
			Now: now, Sleep: sleep,
		})
		close(done)
	}()

	// Push a packet so the inner sender goroutine is blocked in Sleep.
	in <- bellrtp.Packet{Header: rtp.Header{}, Payload: []byte{0xAA}}
	cancel()

	select {
	case <-done:
		// success
	case <-time.After(time.Second):
		t.Fatal("runEchoLoop did not return after ctx cancel")
	}
}

func TestRunEchoLoop_StopsOnInputClose(t *testing.T) {
	t.Parallel()

	in := make(chan bellrtp.Packet)
	close(in) // immediately closed: producer exits, queue closes, consumer exits

	send := func(rtp.Header, []byte) error { return nil }
	clock := time.Now()
	now := func() time.Time { return clock }
	sleep := func(_ context.Context, _ time.Time, _ func() time.Time) {}

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		runEchoLoop(ctx, echoLoopParams{
			In: in, Send: send, Delay: 100 * time.Millisecond,
			PayloadType: 0, SSRC: 1, TSIncrement: 160,
			Now: now, Sleep: sleep,
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runEchoLoop did not return after input close")
	}
}

func TestSplitHostPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		host    string
		port    int
		wantErr bool
	}{
		{"192.168.1.5:30000", "192.168.1.5", 30000, false},
		{"127.0.0.1:5060", "127.0.0.1", 5060, false},
		{"[::1]:5060", "::1", 5060, false},
		{"noport", "", 0, true},
		{"192.168.1.5:0", "", 0, true},
		{"192.168.1.5:99999", "", 0, true},
		{"192.168.1.5:abc", "", 0, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			h, p, err := splitHostPort(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got host=%q port=%d", h, p)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h != c.host || p != c.port {
				t.Errorf("got %q %d, want %q %d", h, p, c.host, c.port)
			}
		})
	}
}

func TestSleepUntil_NoOpInPast(t *testing.T) {
	t.Parallel()
	clock := time.Now()
	start := time.Now()
	sleepUntil(context.Background(), clock.Add(-time.Second), func() time.Time { return clock })
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("sleepUntil(past) blocked for %v", time.Since(start))
	}
}

func TestAdvertisedIPOrLocal(t *testing.T) {
	t.Parallel()
	// External IP set → external wins regardless of socket state.
	if got := advertisedIPOrLocal("203.0.113.42", nil); got != "203.0.113.42" {
		t.Errorf("external precedence: got %q, want 203.0.113.42", got)
	}
	// Nil session, no external → fallback constant.
	if got := advertisedIPOrLocal("", nil); got != "127.0.0.1" {
		t.Errorf("nil-session fallback: got %q, want 127.0.0.1", got)
	}
}

func TestSleepUntil_CancelEndsEarly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	sleepUntil(ctx, time.Now().Add(5*time.Second), time.Now)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("sleepUntil did not honor cancel: elapsed=%v", elapsed)
	}
}
