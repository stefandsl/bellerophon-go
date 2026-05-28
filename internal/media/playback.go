package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pion/rtp"

	"github.com/stefandsl/bellerophon-go/internal/codec"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

const (
	// PtimeMs is the RTP packetization interval for G.711 telephony. Matches
	// what the SDP answer offers (a=ptime:20) and what 3CX / MessageNet /
	// generic registrars all expect.
	PtimeMs = 20
	// SamplesPerFrame is the number of PCM samples in one 20 ms G.711 frame
	// at 8 kHz.
	SamplesPerFrame = PtimeMs * 8
)

// Codec selects the G.711 variant for playback encoding.
type Codec int

const (
	CodecPCMU Codec = iota
	CodecPCMA
)

// PlaybackOptions configures Play. Send and PayloadType are required;
// everything else has a sensible default.
type PlaybackOptions struct {
	// Codec picks the G.711 variant for the wire encoding.
	Codec Codec
	// PayloadType is the RTP PT to stamp on outgoing headers — usually 0
	// (PCMU) or 8 (PCMA). The caller is responsible for matching this to the
	// SDP-negotiated codec.
	PayloadType uint8
	// SSRC is the synchronization source for the outbound stream.
	SSRC uint32
	// StartSeq is the first RTP sequence number; increments by 1 per frame.
	StartSeq uint16
	// StartTS is the first RTP timestamp; increments by SamplesPerFrame.
	StartTS uint32
	// Send transmits one RTP packet. Returning an error aborts playback.
	Send func(rtp.Header, []byte) error
	// FrameInterval is the wall-clock spacing between send attempts. Defaults
	// to PtimeMs * time.Millisecond when zero. Tests pass a tiny value for
	// fast runs.
	FrameInterval time.Duration
	// Logger is optional; if nil, playback is silent.
	Logger bellog.Logger
}

// Play encodes wav as G.711 and streams it as 20 ms RTP frames through
// opts.Send at the configured interval. Returns when:
//   - the audio is exhausted (nil)
//   - ctx is cancelled (ctx.Err())
//   - opts.Send returns an error
//
// Input WAV is normalized to mono 8 kHz before encoding. Sample rates other
// than 8 kHz and 16 kHz are rejected (M001 scope; broader resampling lands
// later if a non-narrowband source ever needs to feed the SIP path).
//
// A trailing partial frame (fewer than SamplesPerFrame samples) is silently
// discarded — RTP frames must be whole 20 ms chunks.
func Play(ctx context.Context, wav *WAV, opts PlaybackOptions) error {
	if opts.Send == nil {
		return errors.New("playback: Send is required")
	}
	if wav == nil {
		return errors.New("playback: wav is nil")
	}

	mono := wav.ToMono()
	samples := mono.Samples
	switch mono.SampleRate {
	case 8000:
		// already at telephony rate
	case 16000:
		samples = codec.Resample16to8(samples)
	default:
		return fmt.Errorf("playback: sample rate %d not supported (need 8000 or 16000)",
			mono.SampleRate)
	}

	g711 := make([]byte, len(samples))
	switch opts.Codec {
	case CodecPCMU:
		codec.EncodePCMUFrame(g711, samples)
	case CodecPCMA:
		codec.EncodePCMAFrame(g711, samples)
	default:
		return fmt.Errorf("playback: unknown codec %d", opts.Codec)
	}

	interval := opts.FrameInterval
	if interval <= 0 {
		interval = PtimeMs * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	seq := opts.StartSeq
	ts := opts.StartTS
	frames := 0

	for offset := 0; offset+SamplesPerFrame <= len(g711); offset += SamplesPerFrame {
		select {
		case <-ctx.Done():
			if opts.Logger != nil {
				opts.Logger.Debug("playback aborted", "frames_sent", frames, "reason", ctx.Err().Error())
			}
			return ctx.Err()
		case <-ticker.C:
		}
		h := rtp.Header{
			Version:        2,
			PayloadType:    opts.PayloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           opts.SSRC,
			Marker:         frames == 0, // first frame of the stream
		}
		if err := opts.Send(h, g711[offset:offset+SamplesPerFrame]); err != nil {
			return fmt.Errorf("playback: send seq=%d: %w", seq, err)
		}
		seq++
		ts += SamplesPerFrame
		frames++
	}

	if opts.Logger != nil {
		opts.Logger.Info("playback complete", "frames", frames, "samples", len(samples))
	}
	return nil
}
