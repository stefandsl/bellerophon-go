package rtp

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// rfc2833Packet returns a 4-byte RFC 2833 payload.
func rfc2833Packet(event byte, end bool, volume uint8, durationTicks uint16) []byte {
	b := make([]byte, 4)
	b[0] = event
	b[1] = volume & 0x3F
	if end {
		b[1] |= 0x80
	}
	binary.BigEndian.PutUint16(b[2:], durationTicks)
	return b
}

func TestParseRFC2833_KnownVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
		want rfc2833Payload
	}{
		{
			name: "digit_5_end_vol_10_duration_800",
			in:   rfc2833Packet(5, true, 10, 800),
			want: rfc2833Payload{Event: 5, End: true, Volume: 10, DurationTicks: 800},
		},
		{
			name: "star_no_end_max_volume",
			in:   rfc2833Packet(10, false, 63, 0),
			want: rfc2833Payload{Event: 10, End: false, Volume: 63, DurationTicks: 0},
		},
		{
			name: "hash_with_R_bit_set",
			in:   []byte{11, 0x40, 0x03, 0x20}, // R=1, vol=0, duration=800
			want: rfc2833Payload{Event: 11, End: false, Reserved: true, Volume: 0, DurationTicks: 800},
		},
		{
			name: "ABCD_event_D_with_padding",
			in:   append(rfc2833Packet(15, true, 5, 1600), 0xFF, 0xFF), // extra bytes ignored
			want: rfc2833Payload{Event: 15, End: true, Volume: 5, DurationTicks: 1600},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRFC2833(c.in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParseRFC2833_RejectsShort(t *testing.T) {
	t.Parallel()
	for n := 0; n < 4; n++ {
		_, err := parseRFC2833(make([]byte, n))
		if err == nil {
			t.Errorf("len=%d: want error, got nil", n)
		}
	}
}

func TestEventToDigit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event byte
		want  byte
		err   bool
	}{
		{0, '0', false},
		{9, '9', false},
		{10, '*', false},
		{11, '#', false},
		{12, 'A', false},
		{15, 'D', false},
		{16, 0, true},  // tone, not DTMF
		{255, 0, true}, // out of range
	}
	for _, c := range cases {
		got, err := eventToDigit(c.event)
		if c.err {
			if err == nil {
				t.Errorf("event=%d: want error", c.event)
			}
			continue
		}
		if err != nil {
			t.Errorf("event=%d: unexpected err %v", c.event, err)
		}
		if got != c.want {
			t.Errorf("event=%d: got %q, want %q", c.event, got, c.want)
		}
	}
}

func TestTicksToDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ticks uint16
		clock uint32
		want  time.Duration
	}{
		{8000, 8000, time.Second},
		{800, 8000, 100 * time.Millisecond},
		{0, 8000, 0},
		{1600, 16000, 100 * time.Millisecond},
		{500, 0, 0}, // guard against div-zero
	}
	for _, c := range cases {
		got := ticksToDuration(c.ticks, c.clock)
		if got != c.want {
			t.Errorf("ticks=%d clock=%d: got %v, want %v", c.ticks, c.clock, got, c.want)
		}
	}
}

// feedKeypress simulates one RFC 2833 keypress on the detector: `updates`
// mid-press packets with the running duration, followed by 3 end packets
// (the RFC-mandated triple-end). Returns the timestamp used so caller can
// advance.
func feedKeypress(d *DTMFDetector, event byte, ts uint32, updates int) {
	const tickPerFrame = 160 // 20 ms @ 8 kHz
	dur := uint16(tickPerFrame)
	for i := 0; i < updates; i++ {
		d.Push(Packet{
			Header:  rtp.Header{Timestamp: ts, PayloadType: DTMFPayloadType},
			Payload: rfc2833Packet(event, false, 10, dur),
		})
		dur += tickPerFrame
	}
	for i := 0; i < 3; i++ {
		d.Push(Packet{
			Header:  rtp.Header{Timestamp: ts, PayloadType: DTMFPayloadType},
			Payload: rfc2833Packet(event, true, 10, dur),
		})
	}
}

// TestDTMFDetector_Accuracy100Keypresses is the spec-mandated accuracy gate:
// 100 simulated keypresses, ≥99% must be detected exactly once each.
func TestDTMFDetector_Accuracy100Keypresses(t *testing.T) {
	t.Parallel()
	// Buffer needs to fit the synthetic burst — production traffic is
	// human-paced (a few keypresses per second) so the default 16 is fine
	// there, but a synchronous test producer can outrun the consumer.
	d := NewDTMFDetector(DTMFDetectorOptions{BufSize: 200})

	const n = 100
	// Mix of digits 0-9, *, #, A-D to exercise eventToDigit.
	digits := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	rng := rand.New(rand.NewPCG(42, 42))

	want := make([]byte, n)
	go func() {
		ts := uint32(1000)
		for i := 0; i < n; i++ {
			ev := digits[rng.IntN(len(digits))]
			want[i], _ = eventToDigit(ev)
			feedKeypress(d, ev, ts, 5)
			ts += 5000 // gap between keypresses, irrelevant
		}
		d.Close()
	}()

	got := make([]byte, 0, n)
	for ev := range d.Events() {
		got = append(got, ev.Digit)
	}
	if len(got) != n {
		t.Errorf("got %d events, want %d (accuracy = %.2f%%)",
			len(got), n, 100*float64(len(got))/float64(n))
	}
	// Verify order + digit match.
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("event %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDTMFDetector_DedupesRedundantEndPackets verifies the 3-end-packet
// pattern produces exactly one DTMFEvent.
func TestDTMFDetector_DedupesRedundantEndPackets(t *testing.T) {
	t.Parallel()
	d := NewDTMFDetector(DTMFDetectorOptions{})
	go func() {
		// 5 end packets at the same timestamp — extreme over-retransmit.
		for i := 0; i < 5; i++ {
			d.Push(Packet{
				Header:  rtp.Header{Timestamp: 9999, PayloadType: DTMFPayloadType},
				Payload: rfc2833Packet(3, true, 10, 800),
			})
		}
		d.Close()
	}()
	count := 0
	for ev := range d.Events() {
		count++
		if ev.Digit != '3' {
			t.Errorf("digit=%q, want '3'", ev.Digit)
		}
	}
	if count != 1 {
		t.Errorf("emitted %d events from 5 redundant end packets, want 1", count)
	}
}

// TestDTMFDetector_IgnoresMidPressUpdates ensures non-end packets alone don't
// trigger an event. Only end-bit packets emit.
func TestDTMFDetector_IgnoresMidPressUpdates(t *testing.T) {
	t.Parallel()
	d := NewDTMFDetector(DTMFDetectorOptions{})
	go func() {
		for i := 0; i < 10; i++ {
			d.Push(Packet{
				Header:  rtp.Header{Timestamp: 7777, PayloadType: DTMFPayloadType},
				Payload: rfc2833Packet(5, false, 10, uint16(160*(i+1))),
			})
		}
		d.Close()
	}()
	count := 0
	for range d.Events() {
		count++
	}
	if count != 0 {
		t.Errorf("emitted %d events with no end-bit packet, want 0", count)
	}
}

// TestDTMFDetector_IgnoresNonDTMFEvents ensures telephone tone events
// (event ≥ 16) are silently absorbed.
func TestDTMFDetector_IgnoresNonDTMFEvents(t *testing.T) {
	t.Parallel()
	d := NewDTMFDetector(DTMFDetectorOptions{})
	go func() {
		// Event 16 = "telephone fl. on", not a DTMF digit.
		d.Push(Packet{
			Header:  rtp.Header{Timestamp: 5555, PayloadType: DTMFPayloadType},
			Payload: rfc2833Packet(16, true, 10, 800),
		})
		d.Close()
	}()
	count := 0
	for range d.Events() {
		count++
	}
	if count != 0 {
		t.Errorf("emitted %d events for non-DTMF tone, want 0", count)
	}
}

// TestDTMFDetector_DurationAndVolumeCarried verifies the metadata round-trip
// on a single keypress.
func TestDTMFDetector_DurationAndVolumeCarried(t *testing.T) {
	t.Parallel()
	d := NewDTMFDetector(DTMFDetectorOptions{ClockRate: 8000})
	const wantTicks = 1600 // 200 ms at 8 kHz
	const wantVolume = 7
	go func() {
		d.Push(Packet{
			Header:  rtp.Header{Timestamp: 1234, PayloadType: DTMFPayloadType},
			Payload: rfc2833Packet(0, true, wantVolume, wantTicks),
		})
		d.Close()
	}()
	ev, ok := <-d.Events()
	if !ok {
		t.Fatal("Events channel closed without an event")
	}
	if ev.Digit != '0' {
		t.Errorf("digit=%q", ev.Digit)
	}
	if ev.Duration != 200*time.Millisecond {
		t.Errorf("duration=%v, want 200ms", ev.Duration)
	}
	if ev.Volume != wantVolume {
		t.Errorf("volume=%d, want %d", ev.Volume, wantVolume)
	}
}

// TestDTMFDetector_CloseIsIdempotent ensures double-close doesn't panic.
func TestDTMFDetector_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	d := NewDTMFDetector(DTMFDetectorOptions{})
	d.Close()
	d.Close() // must not panic
	// Push after close is silently dropped.
	d.Push(Packet{
		Header:  rtp.Header{Timestamp: 1, PayloadType: DTMFPayloadType},
		Payload: rfc2833Packet(1, true, 10, 800),
	})
}
