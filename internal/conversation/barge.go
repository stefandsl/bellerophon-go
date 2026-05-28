package conversation

import (
	"math"
	"sync"
	"time"
)

// EnergyBarge is a lightweight speech-onset detector tuned for the
// barge-in path. Unlike the full [vad.Detector] (S01), this one has
// NO hangover — the first frame whose RMS or peak crosses the
// configured threshold trips a signal on Speech(). That difference
// is deliberate: the VAD finalizes utterances after a long silence
// window (1.5 s by default) so callers don't get split by mid-sentence
// pauses; barge-in needs the opposite, fastest-possible "user started
// talking" detection so we can cut TTS within ~100 ms (the M002 §2
// success criterion).
//
// Wiring is fan-out from the same PCM source feeding the VAD:
//
//	rtp.Receive → PCM16 → vad.Detector.Push    (utterance segmentation)
//	                    └→ barge.Push          (onset for TTS cancel)
//
// Reset() rearms the detector for the next SPEAKING segment. The
// loop calls it on each SPEAKING entry so a signal that fired at the
// end of the previous turn doesn't immediately cancel the next reply.
type EnergyBarge struct {
	rate    int
	rmsThr  float64
	maxThr  float64
	minOnMs int

	mu      sync.Mutex
	armed   bool
	sig     chan struct{}
	onsetMs int
	totalMs int
}

// EnergyBargeOptions configures NewEnergyBarge.
type EnergyBargeOptions struct {
	// SampleRate of input samples. Default 16 kHz to match VAD.
	SampleRate int
	// RMSThreshold above which a chunk counts as speech. Default
	// matches voice-app's barge-in heuristic — slightly higher than
	// the main VAD threshold so background noise / line hum doesn't
	// trip a false barge-in.
	RMSThreshold float64
	// MaxThreshold catches sharp glottal onsets that don't lift the
	// 20 ms RMS yet. Default matches VAD.
	MaxThreshold float64
	// MinOnsetMs requires this many consecutive ms above threshold
	// before tripping. Default 60 ms — enough to reject a single
	// clipped frame from a passing siren without losing perceptible
	// responsiveness. (M002 §2 says ≤100 ms from real speech to
	// playback abort; 60 ms detect + ~ms cancel propagation leaves
	// margin.)
	MinOnsetMs int
}

// Defaults for EnergyBarge. Pulled out so tests can pin them and the
// docstring stays in sync.
const (
	DefaultBargeRMSThreshold = 4000.0
	DefaultBargeMaxThreshold = 10000.0
	DefaultBargeMinOnsetMs   = 60
)

// NewEnergyBarge builds a detector with opts, filling defaults. It
// starts in the disarmed state — the loop calls Reset() once it
// enters SPEAKING.
func NewEnergyBarge(opts EnergyBargeOptions) *EnergyBarge {
	if opts.SampleRate <= 0 {
		opts.SampleRate = 16000
	}
	if opts.RMSThreshold <= 0 {
		opts.RMSThreshold = DefaultBargeRMSThreshold
	}
	if opts.MaxThreshold <= 0 {
		opts.MaxThreshold = DefaultBargeMaxThreshold
	}
	if opts.MinOnsetMs <= 0 {
		opts.MinOnsetMs = DefaultBargeMinOnsetMs
	}
	return &EnergyBarge{
		rate:    opts.SampleRate,
		rmsThr:  opts.RMSThreshold,
		maxThr:  opts.MaxThreshold,
		minOnMs: opts.MinOnsetMs,
	}
}

// Speech returns a channel that closes (broadcast-style) when onset
// is detected. The same channel is returned across calls between
// Reset()s. Reset() swaps in a fresh channel — callers that hold an
// older reference will still see the previous "fire" but won't
// receive a stale signal in the next turn.
func (e *EnergyBarge) Speech() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sig == nil {
		e.sig = make(chan struct{})
	}
	return e.sig
}

// Reset rearms the detector. Channel-fired signals from the previous
// arming are NOT propagated to the new channel — a fresh Speech()
// after Reset returns an unfired channel.
func (e *EnergyBarge) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.armed = true
	e.onsetMs = 0
	e.totalMs = 0
	e.sig = make(chan struct{})
}

// Push feeds PCM16 samples. Safe to call from the same producer
// goroutine that drives vad.Detector.Push — typically the audio
// capture goroutine. Concurrent Push calls are serialized through
// the mutex but assume a single producer in production.
func (e *EnergyBarge) Push(samples []int16) {
	if len(samples) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.armed {
		return
	}

	chunkMs := 1000 * len(samples) / e.rate
	if chunkMs == 0 {
		chunkMs = 1
	}
	e.totalMs += chunkMs

	rms, peak := analyzeBarge(samples)
	isSpeech := rms >= e.rmsThr || peak >= e.maxThr

	if isSpeech {
		e.onsetMs += chunkMs
		if e.onsetMs >= e.minOnMs {
			// Trip and disarm — Reset() must be called to fire again.
			e.armed = false
			if e.sig != nil {
				close(e.sig)
			}
		}
		return
	}
	// Silence resets the in-progress onset window; we want continuous
	// speech, not cumulative bursts of noise spread over seconds.
	e.onsetMs = 0
}

// TotalMs is the number of milliseconds of audio pushed since the
// last Reset. Exposed for tests/instrumentation; production callers
// shouldn't depend on it.
func (e *EnergyBarge) TotalMs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.totalMs
}

// analyzeBarge computes RMS and peak amplitude of samples. Mirrors
// vad.analyze; duplicated here so the barge detector doesn't reach
// into the VAD package's unexported helpers.
func analyzeBarge(samples []int16) (rms, peak float64) {
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

// onsetDeadline returns a time.Duration representing how long PCM
// audio of length n samples at the given sampleRate covers — used by
// tests to assert "speech fires within X ms" claims without sleep
// flakes. Kept here for symmetry with the package.
func onsetDeadline(samples, sampleRate int) time.Duration {
	if sampleRate <= 0 {
		return 0
	}
	return time.Duration(1000*samples/sampleRate) * time.Millisecond
}
