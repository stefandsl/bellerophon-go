// Package vad implements voice-activity detection and utterance
// segmentation for the Bellerophon conversation loop. The algorithm is
// a port of voice-app/lib/audio-fork.js (Node.js stack): per-chunk RMS
// + peak amplitude thresholds with a hangover-style silence detector.
//
// The detector consumes PCM16 mono samples (typically 16 kHz, Whisper's
// native rate) via Push() and emits one Utterance per detected speech
// segment on Utterances(). It is the first stage of the M002 inbound
// conversation pipeline: STT, LLM, and TTS sit downstream of the
// utterance channel.
package vad

import (
	"math"
	"sync"
	"time"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

// Defaults mirror voice-app's audio-fork.js so swapping the Go binary
// in for the Node stack does not change perceived call dynamics.
const (
	// DefaultSampleRate is Whisper's native rate.
	DefaultSampleRate = 16000
	// DefaultRMSThreshold — frames whose RMS amplitude (in int16 units)
	// is at or above this value are treated as speech.
	DefaultRMSThreshold = 3000
	// DefaultMaxThreshold catches sharp onsets that don't yet show up
	// in the 20 ms RMS — a glottal attack on the first frame of a word.
	DefaultMaxThreshold = 8000
	// DefaultEndSilenceMs is the consecutive-silence window after which
	// an in-progress utterance is finalized. Matches voice-app's 1.5 s.
	DefaultEndSilenceMs = 1500
	// DefaultMinSpeechMs is the minimum accumulated speech needed before
	// a stretch is considered an utterance (false-start filter).
	DefaultMinSpeechMs = 350
)

// Options configures NewDetector. Zero values pick the defaults above,
// so an Options{} with just SampleRate set is a sensible baseline.
type Options struct {
	SampleRate   int           // input PCM rate; default DefaultSampleRate
	RMSThreshold float64       // default DefaultRMSThreshold
	MaxThreshold float64       // default DefaultMaxThreshold
	EndSilenceMs int           // default DefaultEndSilenceMs
	MinSpeechMs  int           // default DefaultMinSpeechMs
	BufSize      int           // Utterances() channel capacity; default 16
	Logger       bellog.Logger // optional; nil is silent
}

// Utterance is one detected segment of speech, ready to feed into STT.
// The PCM16 slice carries the raw samples including the configured
// hangover tail — by design, so downstream Whisper has a few hundred ms
// of post-speech context to anchor its tokenizer.
type Utterance struct {
	PCM16      []int16
	DurationMs int
	StartedAt  time.Time
	EndedAt    time.Time
}

// Detector runs PCM16 samples through an RMS+peak VAD and emits one
// Utterance per detected segment. Push() is the only mutator and is
// safe to call from a single producer goroutine (the audio capture
// path). Utterances() is consumed from another goroutine.
type Detector struct {
	rate         int
	rmsThr       float64
	maxThr       float64
	endSilenceMs int
	minSpeechMs  int
	logger       bellog.Logger

	out chan Utterance

	mu       sync.Mutex
	closed   bool
	buf      []int16   // accumulated samples for current utterance
	started  time.Time // start instant of current utterance (zero if none)
	speechMs int       // accumulated speech ms in the current attempt
	silentMs int       // consecutive silent ms since last speech frame
	inSpeech bool      // true once speechMs has crossed minSpeechMs
}

// NewDetector builds a detector with opts, filling defaults for zero
// fields.
func NewDetector(opts Options) *Detector {
	if opts.SampleRate <= 0 {
		opts.SampleRate = DefaultSampleRate
	}
	if opts.RMSThreshold <= 0 {
		opts.RMSThreshold = DefaultRMSThreshold
	}
	if opts.MaxThreshold <= 0 {
		opts.MaxThreshold = DefaultMaxThreshold
	}
	if opts.EndSilenceMs <= 0 {
		opts.EndSilenceMs = DefaultEndSilenceMs
	}
	if opts.MinSpeechMs <= 0 {
		opts.MinSpeechMs = DefaultMinSpeechMs
	}
	bufSize := opts.BufSize
	if bufSize <= 0 {
		bufSize = 16
	}
	return &Detector{
		rate:         opts.SampleRate,
		rmsThr:       opts.RMSThreshold,
		maxThr:       opts.MaxThreshold,
		endSilenceMs: opts.EndSilenceMs,
		minSpeechMs:  opts.MinSpeechMs,
		logger:       opts.Logger,
		out:          make(chan Utterance, bufSize),
	}
}

// Utterances returns the channel on which detected speech segments
// arrive. The channel closes when Close is called.
func (d *Detector) Utterances() <-chan Utterance { return d.out }

// Close drains any partially-buffered speech (emits a final utterance
// if one was in progress past the minSpeechMs threshold) and closes
// the Utterances channel. Safe to call multiple times.
func (d *Detector) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	if d.inSpeech && len(d.buf) > 0 {
		d.finalizeLocked()
	}
	d.mu.Unlock()
	close(d.out)
}

// Push feeds a chunk of PCM16 samples into the detector. The chunk size
// can vary — typical telephony chunks are 20 ms (320 samples @ 16 kHz)
// but any non-empty slice works. Push is a no-op after Close.
func (d *Detector) Push(samples []int16) {
	if len(samples) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}

	chunkMs := 1000 * len(samples) / d.rate
	if chunkMs == 0 {
		chunkMs = 1 // tiny chunks below 1 ms still get accounted as 1 ms
	}
	rms, peak := analyze(samples)

	// Frame is "speech" if EITHER RMS is high OR peak amplitude is high.
	// Catches glottal-attack onsets that don't yet show up in 20 ms RMS.
	isSpeech := rms >= d.rmsThr || peak >= d.maxThr

	if isSpeech {
		if d.started.IsZero() {
			d.started = time.Now()
		}
		d.buf = append(d.buf, samples...)
		d.speechMs += chunkMs
		d.silentMs = 0
		if !d.inSpeech && d.speechMs >= d.minSpeechMs {
			d.inSpeech = true
		}
		return
	}

	// Silent frame.
	if d.started.IsZero() {
		// Pre-speech silence — discard, don't accumulate.
		return
	}
	// We're past the (provisional) onset. Keep buffering for the
	// hangover window so cropped utterances don't lose their tails.
	d.buf = append(d.buf, samples...)
	d.silentMs += chunkMs
	if d.silentMs < d.endSilenceMs {
		return
	}
	// Hangover elapsed.
	if d.inSpeech {
		d.finalizeLocked()
	} else {
		// Provisional speech never crossed minSpeechMs — false start.
		d.reset()
	}
}

func (d *Detector) finalizeLocked() {
	durationMs := 1000 * len(d.buf) / d.rate
	ut := Utterance{
		PCM16:      d.buf,
		DurationMs: durationMs,
		StartedAt:  d.started,
		EndedAt:    time.Now(),
	}
	select {
	case d.out <- ut:
	default:
		if d.logger != nil {
			d.logger.Warn("vad channel full, dropping utterance",
				"duration_ms", durationMs)
		}
	}
	d.reset()
}

func (d *Detector) reset() {
	d.buf = nil
	d.started = time.Time{}
	d.speechMs = 0
	d.silentMs = 0
	d.inSpeech = false
}

// analyze computes RMS and peak amplitude of a sample slice. Returns
// 0,0 for an empty input (caller's responsibility to guard, but we
// don't divide by zero either way).
func analyze(samples []int16) (rms, peak float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	var sumSq float64
	var maxAbs int32
	for _, s := range samples {
		f := float64(s)
		sumSq += f * f
		abs := int32(s)
		if abs < 0 {
			abs = -abs
		}
		if abs > maxAbs {
			maxAbs = abs
		}
	}
	rms = math.Sqrt(sumSq / float64(len(samples)))
	peak = float64(maxAbs)
	return rms, peak
}
