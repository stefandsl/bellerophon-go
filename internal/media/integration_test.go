package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/stefandsl/bellerophon-go/internal/codec"
)

// TestKnownWAVProducesExpectedRTPSequence is the S05 integration must-have:
// build a real RIFF/WAVE byte stream, parse it through ReadWAV, play it
// through Play, and verify the resulting RTP packets exactly match what a
// hand-computed encoder would produce — packet count, payload type,
// sequence numbers, timestamp deltas, marker bit, and the first few wire
// bytes against codec.EncodePCMU.
func TestKnownWAVProducesExpectedRTPSequence(t *testing.T) {
	t.Parallel()

	// Synthesise 100 ms of 1 kHz sine at 8 kHz mono 16-bit. That's
	// 800 samples = 5 × 160-sample G.711 frames.
	const fs = 8000
	const fSig = 1000.0
	const seconds = 0.1
	const amp = 5000.0
	n := int(fs * seconds)
	samples := make([]int16, n)
	for i := range samples {
		samples[i] = int16(amp * math.Sin(2*math.Pi*fSig*float64(i)/fs))
	}

	// Pack into a real WAV byte stream and parse it back.
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s)) //nolint:gosec // int16 reinterpret
	}
	wavBytes := buildWAV(t, 1, 8000, 16, data)
	parsed, err := ReadWAV(bytes.NewReader(wavBytes))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}

	// Play through a capturing sender.
	send, recs := captureSender()
	err = Play(context.Background(), parsed, PlaybackOptions{
		Codec:         CodecPCMU,
		PayloadType:   0,
		SSRC:          0xDEADBEEF,
		StartSeq:      42,
		StartTS:       1_000_000,
		Send:          send,
		FrameInterval: 1 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}

	got := recs()
	const wantFrames = 5
	if len(got) != wantFrames {
		t.Fatalf("frames sent = %d, want %d", len(got), wantFrames)
	}

	// Hand-compute the expected G.711 bytes for the first frame and
	// compare to what Play actually sent.
	expectedFirstFrame := make([]byte, SamplesPerFrame)
	codec.EncodePCMUFrame(expectedFirstFrame, samples[:SamplesPerFrame])
	if !bytes.Equal(got[0].payload, expectedFirstFrame) {
		t.Errorf("frame 0 wire bytes mismatch:\n got %x\n want %x",
			got[0].payload, expectedFirstFrame)
	}

	// Verify every header field across the whole stream.
	for i, r := range got {
		if r.hdr.Version != 2 {
			t.Errorf("frame %d Version=%d, want 2", i, r.hdr.Version)
		}
		if r.hdr.PayloadType != 0 {
			t.Errorf("frame %d PT=%d, want 0", i, r.hdr.PayloadType)
		}
		if r.hdr.SSRC != 0xDEADBEEF {
			t.Errorf("frame %d SSRC=%#x", i, r.hdr.SSRC)
		}
		wantSeq := uint16(42 + i) //nolint:gosec // bounded
		if r.hdr.SequenceNumber != wantSeq {
			t.Errorf("frame %d seq=%d, want %d", i, r.hdr.SequenceNumber, wantSeq)
		}
		wantTS := uint32(1_000_000 + i*SamplesPerFrame) //nolint:gosec // bounded
		if r.hdr.Timestamp != wantTS {
			t.Errorf("frame %d ts=%d, want %d", i, r.hdr.Timestamp, wantTS)
		}
		if i == 0 != r.hdr.Marker {
			t.Errorf("frame %d marker=%v, want %v", i, r.hdr.Marker, i == 0)
		}
		if len(r.payload) != SamplesPerFrame {
			t.Errorf("frame %d payload len=%d, want %d", i, len(r.payload), SamplesPerFrame)
		}
	}
}

// BenchmarkPlay_EncodeAndSend measures the encode+send throughput end-to-end.
// Target per M001-SPEC §5 S04 must-haves: >100x realtime on a Pi 4. With
// FrameInterval=0 (defaults to 20ms ticker) we'd be wall-clock-bound; instead
// we use a microsecond interval so the benchmark measures CPU work only.
func BenchmarkPlay_EncodeAndSend(b *testing.B) {
	// 1 s of 8 kHz mono — 50 frames worth of work per iteration.
	samples := make([]int16, 8000)
	for i := range samples {
		samples[i] = int16(i * 100) //nolint:gosec // bounded
	}
	wav := mono8kWAV(samples)
	send := func(rtp.Header, []byte) error { return nil }

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Play(context.Background(), wav, PlaybackOptions{
			Codec:         CodecPCMU,
			Send:          send,
			FrameInterval: 1 * time.Microsecond,
		})
	}
}
