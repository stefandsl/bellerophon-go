package media

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// sentRecord captures a single Send call for inspection.
type sentRecord struct {
	hdr     rtp.Header
	payload []byte
}

// captureSender returns a Send func + thread-safe accessor for what was sent.
func captureSender() (func(rtp.Header, []byte) error, func() []sentRecord) {
	var (
		mu   sync.Mutex
		recs []sentRecord
	)
	send := func(h rtp.Header, p []byte) error {
		mu.Lock()
		recs = append(recs, sentRecord{hdr: h, payload: append([]byte(nil), p...)})
		mu.Unlock()
		return nil
	}
	get := func() []sentRecord {
		mu.Lock()
		defer mu.Unlock()
		out := make([]sentRecord, len(recs))
		copy(out, recs)
		return out
	}
	return send, get
}

func mono8kWAV(samples []int16) *WAV {
	return &WAV{SampleRate: 8000, Channels: 1, BitDepth: 16, Samples: samples}
}

func TestPlay_EmitsExpectedRTPFrames(t *testing.T) {
	t.Parallel()
	// 5 frames worth of audio (800 samples = 5 × 160).
	samples := make([]int16, 5*SamplesPerFrame)
	for i := range samples {
		samples[i] = int16(i % 1000) //nolint:gosec // bounded
	}
	send, recs := captureSender()

	err := Play(context.Background(), mono8kWAV(samples), PlaybackOptions{
		Codec:         CodecPCMU,
		PayloadType:   0,
		SSRC:          0xCAFE,
		StartSeq:      100,
		StartTS:       1000,
		Send:          send,
		FrameInterval: 1 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}

	got := recs()
	if len(got) != 5 {
		t.Fatalf("frames sent = %d, want 5", len(got))
	}
	for i, r := range got {
		if r.hdr.PayloadType != 0 {
			t.Errorf("frame %d PT=%d, want 0", i, r.hdr.PayloadType)
		}
		if r.hdr.SSRC != 0xCAFE {
			t.Errorf("frame %d SSRC=%#x", i, r.hdr.SSRC)
		}
		wantSeq := uint16(100 + i) //nolint:gosec // bounded
		if r.hdr.SequenceNumber != wantSeq {
			t.Errorf("frame %d seq=%d, want %d", i, r.hdr.SequenceNumber, wantSeq)
		}
		wantTS := uint32(1000 + i*SamplesPerFrame) //nolint:gosec // bounded
		if r.hdr.Timestamp != wantTS {
			t.Errorf("frame %d ts=%d, want %d", i, r.hdr.Timestamp, wantTS)
		}
		if i == 0 && !r.hdr.Marker {
			t.Error("first frame should have marker bit set")
		}
		if i > 0 && r.hdr.Marker {
			t.Errorf("frame %d should not have marker bit", i)
		}
		if len(r.payload) != SamplesPerFrame {
			t.Errorf("frame %d payload len=%d, want %d", i, len(r.payload), SamplesPerFrame)
		}
	}
}

func TestPlay_DropsTrailingPartialFrame(t *testing.T) {
	t.Parallel()
	// 2.5 frames worth — should send 2 and drop the trailing 80 samples.
	samples := make([]int16, 2*SamplesPerFrame+SamplesPerFrame/2)
	send, recs := captureSender()

	err := Play(context.Background(), mono8kWAV(samples), PlaybackOptions{
		Codec:         CodecPCMU,
		Send:          send,
		FrameInterval: 1 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if got := len(recs()); got != 2 {
		t.Errorf("frames sent = %d, want 2 (trailing partial dropped)", got)
	}
}

func TestPlay_AbortsOnContextCancel(t *testing.T) {
	t.Parallel()
	// 100 frames worth at slow tick so cancel definitively interrupts.
	samples := make([]int16, 100*SamplesPerFrame)
	send, recs := captureSender()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Play(ctx, mono8kWAV(samples), PlaybackOptions{
			Codec:         CodecPCMU,
			Send:          send,
			FrameInterval: 5 * time.Millisecond,
		})
	}()

	// Let a few frames go out.
	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil || err.Error() != "context canceled" {
			t.Errorf("expected context canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Play did not return after cancel")
	}

	sent := len(recs())
	if sent == 0 {
		t.Error("Play exited before sending any frames")
	}
	if sent >= 100 {
		t.Errorf("Play sent %d/100 frames; cancel didn't interrupt", sent)
	}
}

func TestPlay_RejectsMissingSend(t *testing.T) {
	t.Parallel()
	err := Play(context.Background(), mono8kWAV(nil), PlaybackOptions{})
	if err == nil || err.Error() != "playback: Send is required" {
		t.Errorf("expected missing-Send error, got %v", err)
	}
}

func TestPlay_RejectsNilWAV(t *testing.T) {
	t.Parallel()
	send, _ := captureSender()
	err := Play(context.Background(), nil, PlaybackOptions{Send: send})
	if err == nil {
		t.Fatal("expected nil-wav error")
	}
}

func TestPlay_RejectsUnsupportedSampleRate(t *testing.T) {
	t.Parallel()
	send, _ := captureSender()
	bad := &WAV{SampleRate: 44100, Channels: 1, BitDepth: 16, Samples: make([]int16, 160)}
	err := Play(context.Background(), bad, PlaybackOptions{Codec: CodecPCMU, Send: send})
	if err == nil {
		t.Fatal("expected unsupported-rate error")
	}
}

func TestPlay_Resamples16kInputTo8k(t *testing.T) {
	t.Parallel()
	// 1 second of 16 kHz mono audio → after Resample16to8 we expect 8000
	// samples = 50 frames.
	samples := make([]int16, 16000)
	for i := range samples {
		samples[i] = int16(i % 500) //nolint:gosec // bounded
	}
	send, recs := captureSender()
	err := Play(context.Background(), &WAV{
		SampleRate: 16000, Channels: 1, BitDepth: 16, Samples: samples,
	}, PlaybackOptions{
		Codec: CodecPCMU, Send: send,
		FrameInterval: 1 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	frames := len(recs())
	if frames != 50 {
		t.Errorf("frames = %d, want 50 (1 s @ 8 kHz / 20 ms)", frames)
	}
}

func TestPlay_PCMAEncodesDifferentBytesThanPCMU(t *testing.T) {
	t.Parallel()
	// Same input through PCMU and PCMA should produce different wire bytes.
	samples := make([]int16, SamplesPerFrame)
	for i := range samples {
		samples[i] = int16(i * 200) //nolint:gosec // bounded
	}

	send1, recs1 := captureSender()
	err := Play(context.Background(), mono8kWAV(samples), PlaybackOptions{
		Codec: CodecPCMU, Send: send1, FrameInterval: 1 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("PCMU: %v", err)
	}
	send2, recs2 := captureSender()
	err = Play(context.Background(), mono8kWAV(samples), PlaybackOptions{
		Codec: CodecPCMA, Send: send2, FrameInterval: 1 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("PCMA: %v", err)
	}

	r1 := recs1()
	r2 := recs2()
	if len(r1) != 1 || len(r2) != 1 {
		t.Fatalf("want 1 frame each, got %d / %d", len(r1), len(r2))
	}
	identical := true
	for i := range r1[0].payload {
		if r1[0].payload[i] != r2[0].payload[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Error("PCMU and PCMA produced identical wire bytes — codec switch is a no-op")
	}
}
