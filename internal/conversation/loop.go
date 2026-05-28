// Package conversation hosts the inbound-call state machine that
// stitches VAD, STT, LLM and TTS together: the brain of the M002
// voice-agent. It is a Go port of voice-app/lib/conversation-loop.js,
// scoped to the M002 success criteria — single-language operation,
// no DTMF language picker, no echo-confirm masking, no sales-mode.
// Those will land in M003+ as additive pieces alongside this loop,
// not by reshaping it.
//
// # State machine
//
//	IDLE
//	  └─▶ LISTENING ◀───────────────┐
//	         │                       │
//	         ▼                       │
//	      TRANSCRIBING               │  ← barge-in (speech detected
//	         │                       │     during SPEAKING aborts
//	         ▼                       │     playback and re-enters
//	       THINKING                  │     LISTENING with the new
//	         │                       │     utterance pre-queued)
//	         ▼                       │
//	       SPEAKING ─────────────────┘
//	         │
//	         ▼
//	       HANGUP
//
// Each turn the loop:
//
//  1. Waits for one [vad.Utterance] from the audio source. Times out
//     after Options.UtteranceTimeout (replays the noSpeech phrase).
//  2. Transcribes via [stt.Provider].
//  3. Detects goodbye keywords (IsGoodbye). On match, plays the
//     localised farewell and transitions to HANGUP.
//  4. Queries the [llm.Client]. Errors fall back to the localised
//     "bridgeUnknown" phrase rather than crashing the call.
//  5. Synthesizes the reply via [tts.Provider] and pushes it to the
//     [Player]. While SPEAKING, the loop spawns a watcher goroutine
//     that monitors the barge-in detector — first onset cancels the
//     play context and yields control back to LISTENING.
//
// Transcript and turn-count enforcement happen around the steps above;
// see TranscriptWriter and Options.MaxTurns.
package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stefandsl/bellerophon-go/internal/llm"
	bellog "github.com/stefandsl/bellerophon-go/internal/log"
	"github.com/stefandsl/bellerophon-go/internal/stt"
	"github.com/stefandsl/bellerophon-go/internal/tts"
	"github.com/stefandsl/bellerophon-go/internal/vad"
)

// State is one node in the conversation state machine.
type State int

const (
	StateIdle State = iota
	StateListening
	StateTranscribing
	StateThinking
	StateSpeaking
	StateHangup
)

// String renders a State value for logs. Avoids stringer (zero-tool
// dependency); the names are short on purpose so log lines stay
// readable.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "IDLE"
	case StateListening:
		return "LISTENING"
	case StateTranscribing:
		return "TRANSCRIBING"
	case StateThinking:
		return "THINKING"
	case StateSpeaking:
		return "SPEAKING"
	case StateHangup:
		return "HANGUP"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// UtteranceSource is anything that hands the loop a stream of speech
// segments. In production this is *vad.Detector via UtteranceChannel
// (below); in tests it is a stub that injects scripted utterances.
type UtteranceSource interface {
	// Utterances returns the channel the loop ranges over. Closing
	// the channel is interpreted as "audio has ended" and drives the
	// loop to HANGUP at the next decision point.
	Utterances() <-chan vad.Utterance
}

// Player accepts a buffer of mono PCM16 samples at the given rate and
// streams it to the remote peer (RTP, FreeSWITCH, a test fake, …).
// Play must respect ctx — that is how barge-in stops audio mid-frame.
// Implementations should return ctx.Err() when cancelled so callers
// can distinguish "I told you to stop" from a real failure.
type Player interface {
	Play(ctx context.Context, pcm []byte, sampleRate int) error
}

// BargeIn signals that the caller started speaking. Speech() returns
// a channel that fires when an onset is detected; it must be safe to
// call repeatedly across turns (the loop subscribes anew each time it
// enters SPEAKING). Reset() is invoked when the loop leaves SPEAKING
// so the next subscription doesn't fire on stale energy.
type BargeIn interface {
	Speech() <-chan struct{}
	Reset()
}

// Options configures a [Loop]. Many of the verbose defaults — the
// canned phrases, the goodbye keyword list — are loaded from a
// [Strings] value so an operator can swap "Hello!" for "Bonjour !"
// without recompiling. CallID, VAD, STT, LLM, TTS and Player are
// required; everything else has a sensible default.
type Options struct {
	// CallID is the conversation key. Threaded into the LLM client
	// for session resume and used as the JSONL transcript basename.
	// Required.
	CallID string

	// VAD is the source of [vad.Utterance]. Required. The loop only
	// reads from VAD.Utterances() and does not call Push — the audio
	// capture goroutine is the producer.
	VAD UtteranceSource

	// STT, LLM, TTS, Player are the four backend dependencies. All
	// required.
	STT    stt.Provider
	LLM    llm.Client
	TTS    tts.Provider
	Player Player

	// BargeIn is optional — if nil, playback completes without
	// interruption. Production wires a *EnergyBarge consuming the
	// same PCM stream as VAD; tests inject a fake to drive deterministic
	// onset events.
	BargeIn BargeIn

	// Strings holds the per-language canned phrases (greeting,
	// noSpeech, clarify, goodbye, …). Nil → DefaultEnglishStrings()
	// — M002 ships single-language; the multi-language DTMF picker
	// lands in M003.
	Strings *Strings

	// Persona is the system prompt passed to the LLM on the first
	// turn. Empty means "let the model behave as it would by default".
	Persona string

	// VoiceID is the TTS provider voice (ElevenLabs voice id today).
	// Empty asks the provider for its configured default.
	VoiceID string

	// MaxTurns caps the conversation. 0 → DefaultMaxTurns. After
	// the cap is hit the loop plays Strings.MaxTurns and exits.
	MaxTurns int

	// UtteranceTimeout is the wall-clock cap on a single LISTENING
	// step. 0 → DefaultUtteranceTimeout. On timeout the loop plays
	// Strings.NoSpeech and re-enters LISTENING.
	UtteranceTimeout time.Duration

	// LLMTimeout caps a single LLM Query call. 0 → DefaultLLMTimeout.
	LLMTimeout time.Duration

	// TTSTimeout caps a single TTS Synthesize call. 0 → DefaultTTSTimeout.
	TTSTimeout time.Duration

	// TranscriptDir, when non-empty, writes a per-call JSONL file
	// (transcripts/<CallID>.jsonl by default; here the file is
	// <TranscriptDir>/<CallID>.jsonl). Empty means "no transcript on
	// disk" — the conversation still runs.
	TranscriptDir string

	// Logger is optional; nil → silent.
	Logger bellog.Logger
}

// Defaults pulled out as constants so tests don't have to invent them
// and so the conversation-loop spec in M002-DRAFT lines up with the
// code one-to-one.
const (
	DefaultMaxTurns         = 20
	DefaultUtteranceTimeout = 30 * time.Second
	DefaultLLMTimeout       = 30 * time.Second
	DefaultTTSTimeout       = 30 * time.Second
	DefaultPCMSampleRate    = 16000 // mono PCM16 LE — voice-stack lingua franca
)

// Loop is the state-machine driver. One Loop per call; not reusable
// once Run returns. Loop's only public method is Run — the state
// machine is internal so test code can't accidentally call
// transitions out of order.
type Loop struct {
	opts    Options
	strings Strings

	// state is the current node. Updated only from Run (single
	// goroutine), so a plain field is fine without synchronisation.
	state State

	// turn counts user turns (incremented after a successful
	// transcription). Used for the MaxTurns cap and the transcript
	// "turn" field.
	turn int

	writer *TranscriptWriter
}

// New builds a Loop. Returns an error if any required option is
// missing — fail fast at construction beats a nil-deref deep in the
// state machine.
func New(opts Options) (*Loop, error) {
	if opts.CallID == "" {
		return nil, errors.New("conversation: CallID is required")
	}
	if opts.VAD == nil {
		return nil, errors.New("conversation: VAD is required")
	}
	if opts.STT == nil {
		return nil, errors.New("conversation: STT is required")
	}
	if opts.LLM == nil {
		return nil, errors.New("conversation: LLM is required")
	}
	if opts.TTS == nil {
		return nil, errors.New("conversation: TTS is required")
	}
	if opts.Player == nil {
		return nil, errors.New("conversation: Player is required")
	}
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = DefaultMaxTurns
	}
	if opts.UtteranceTimeout <= 0 {
		opts.UtteranceTimeout = DefaultUtteranceTimeout
	}
	if opts.LLMTimeout <= 0 {
		opts.LLMTimeout = DefaultLLMTimeout
	}
	if opts.TTSTimeout <= 0 {
		opts.TTSTimeout = DefaultTTSTimeout
	}
	s := DefaultEnglishStrings()
	if opts.Strings != nil {
		s = *opts.Strings
	}

	var writer *TranscriptWriter
	if opts.TranscriptDir != "" {
		w, err := NewTranscriptWriter(opts.TranscriptDir, opts.CallID)
		if err != nil {
			return nil, fmt.Errorf("conversation: transcript: %w", err)
		}
		writer = w
	}

	return &Loop{
		opts:    opts,
		strings: s,
		state:   StateIdle,
		writer:  writer,
	}, nil
}

// State returns the current state. Safe to call from outside Run —
// not synchronised but the value is read atomically as a plain int.
// Exposed for tests and for an eventual /metrics-style export.
func (l *Loop) State() State { return l.state }

// Turn returns the number of user turns completed.
func (l *Loop) Turn() int { return l.turn }

// Run drives the state machine until ctx ends, a goodbye phrase is
// detected, MaxTurns is exhausted, or the UtteranceSource closes.
// Errors are returned for unrecoverable failures only; per-turn
// upstream errors (LLM unreachable, TTS rate-limited) are handled in
// place by playing the localised fallback phrase.
//
// The caller is expected to drive the audio capture loop (Push to
// VAD and BargeIn) in a separate goroutine — Run does not own audio
// transport.
func (l *Loop) Run(ctx context.Context) error {
	defer l.cleanup()

	if err := l.playGreeting(ctx); err != nil {
		return err
	}

	l.transition(StateListening)
	for l.state != StateHangup {
		if err := ctx.Err(); err != nil {
			l.opts.logf().Info("conversation: ctx ended", "call_id", l.opts.CallID, "err", err.Error())
			return err
		}
		if l.turn >= l.opts.MaxTurns {
			l.playPhrase(ctx, l.strings.MaxTurns)
			l.transition(StateHangup)
			break
		}

		ut, ok := l.waitUtterance(ctx)
		if !ok {
			// Source closed → terminal.
			l.transition(StateHangup)
			break
		}
		if ut == nil {
			// Timeout — prompt and try again.
			l.playPhrase(ctx, l.strings.NoSpeech)
			continue
		}

		l.transition(StateTranscribing)
		transcript, err := l.transcribe(ctx, *ut)
		if err != nil {
			l.opts.logf().Warn("conversation: stt failed",
				"call_id", l.opts.CallID, "err", err.Error())
			l.playPhrase(ctx, l.strings.Clarify)
			l.transition(StateListening)
			continue
		}
		if len(strings.TrimSpace(transcript)) < 2 {
			l.playPhrase(ctx, l.strings.Clarify)
			l.transition(StateListening)
			continue
		}

		l.turn++
		l.writeTranscript("user", transcript)

		if IsGoodbye(transcript) {
			l.playPhrase(ctx, l.strings.Goodbye)
			l.transition(StateHangup)
			break
		}

		l.transition(StateThinking)
		reply := l.query(ctx, transcript)

		// Even if the LLM failed and we fell back to a canned phrase,
		// persist the actual spoken reply — keeps the transcript an
		// honest record of the audio that hit the wire.
		l.writeTranscript("assistant", reply)

		l.transition(StateSpeaking)
		l.speak(ctx, reply)
		l.transition(StateListening)
	}
	return nil
}

// playGreeting renders the configured greeting once on startup. A
// failure here is logged but the loop continues — the caller might
// still be able to hear utterances even if the greeting TTS broke.
func (l *Loop) playGreeting(ctx context.Context) error {
	greeting := l.strings.Greeting
	if greeting == "" {
		return nil
	}
	l.transition(StateSpeaking)
	defer l.transition(StateIdle)
	l.playPhrase(ctx, greeting)
	return nil
}

// waitUtterance returns (utterance, true) on a new utterance,
// (nil, true) on timeout, or (nil, false) if the source channel
// closed.
func (l *Loop) waitUtterance(ctx context.Context) (*vad.Utterance, bool) {
	timer := time.NewTimer(l.opts.UtteranceTimeout)
	defer timer.Stop()

	ch := l.opts.VAD.Utterances()
	select {
	case <-ctx.Done():
		return nil, false
	case ut, ok := <-ch:
		if !ok {
			return nil, false
		}
		return &ut, true
	case <-timer.C:
		return nil, true
	}
}

func (l *Loop) transcribe(ctx context.Context, ut vad.Utterance) (string, error) {
	rate := DefaultPCMSampleRate
	return l.opts.STT.Transcribe(ctx, ut.PCM16, rate)
}

// query runs the LLM and returns the text to speak. On error it
// returns the localised "bridgeUnknown" — never crashes the loop on
// a transient backend failure.
func (l *Loop) query(ctx context.Context, transcript string) string {
	qctx, cancel := context.WithTimeout(ctx, l.opts.LLMTimeout)
	defer cancel()

	resp, err := l.opts.LLM.Query(qctx, llm.Request{
		CallID:       l.opts.CallID,
		Prompt:       transcript,
		DevicePrompt: l.opts.Persona,
	})
	if err != nil {
		l.opts.logf().Warn("conversation: llm failed",
			"call_id", l.opts.CallID, "err", err.Error())
		return l.strings.BridgeUnknown
	}
	if resp.Text == "" {
		return l.strings.BridgeUnknown
	}
	return resp.Text
}

// speak synthesizes text and pushes it to the Player. Barge-in: if
// the BargeIn detector fires while playback is running, the play
// context is cancelled and the function returns — Run then transitions
// back to LISTENING. The next utterance (the one that triggered the
// barge-in) will be picked up off the VAD channel in the next iter.
func (l *Loop) speak(ctx context.Context, text string) {
	if text == "" {
		return
	}
	tctx, tcancel := context.WithTimeout(ctx, l.opts.TTSTimeout)
	pcm, err := l.opts.TTS.Synthesize(tctx, text, l.opts.VoiceID)
	tcancel()
	if err != nil {
		l.opts.logf().Warn("conversation: tts failed",
			"call_id", l.opts.CallID, "err", err.Error())
		return
	}
	if len(pcm) == 0 {
		return
	}

	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()

	if l.opts.BargeIn != nil {
		l.opts.BargeIn.Reset()
		go func() {
			select {
			case <-pctx.Done():
				return
			case <-l.opts.BargeIn.Speech():
				l.opts.logf().Info("conversation: barge-in", "call_id", l.opts.CallID)
				pcancel()
			}
		}()
	}

	if err := l.opts.Player.Play(pctx, pcm, DefaultPCMSampleRate); err != nil &&
		!errors.Is(err, context.Canceled) {
		l.opts.logf().Warn("conversation: player failed",
			"call_id", l.opts.CallID, "err", err.Error())
	}
}

func (l *Loop) playPhrase(ctx context.Context, phrase string) {
	if phrase == "" {
		return
	}
	tctx, tcancel := context.WithTimeout(ctx, l.opts.TTSTimeout)
	pcm, err := l.opts.TTS.Synthesize(tctx, phrase, l.opts.VoiceID)
	tcancel()
	if err != nil {
		l.opts.logf().Warn("conversation: canned phrase tts failed",
			"call_id", l.opts.CallID, "err", err.Error())
		return
	}
	if err := l.opts.Player.Play(ctx, pcm, DefaultPCMSampleRate); err != nil &&
		!errors.Is(err, context.Canceled) {
		l.opts.logf().Warn("conversation: canned phrase play failed",
			"call_id", l.opts.CallID, "err", err.Error())
	}
}

func (l *Loop) writeTranscript(role, text string) {
	if l.writer == nil {
		return
	}
	if err := l.writer.Append(Turn{
		Turn:        l.turn,
		Role:        role,
		Text:        text,
		TimestampMs: time.Now().UnixMilli(),
	}); err != nil {
		l.opts.logf().Warn("conversation: transcript append failed",
			"call_id", l.opts.CallID, "err", err.Error())
	}
}

func (l *Loop) transition(next State) {
	prev := l.state
	l.state = next
	if l.opts.Logger != nil && prev != next {
		l.opts.Logger.Debug("conversation: state",
			"call_id", l.opts.CallID, "from", prev, "to", next, "turn", l.turn)
	}
}

// cleanup releases per-call resources: ends the LLM session and
// closes the transcript file. Called via defer so it runs on the
// normal Run return AND on a ctx-cancelled return.
func (l *Loop) cleanup() {
	// EndSession with the background ctx — the conversation context
	// may already be cancelled, but we still want to flush the
	// upstream cleanup.
	if l.opts.LLM != nil {
		_ = l.opts.LLM.EndSession(context.Background(), l.opts.CallID)
	}
	if l.writer != nil {
		_ = l.writer.Close()
	}
}

// logf returns a non-nil Logger so call sites don't need to nil-check.
func (o Options) logf() bellog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return noopLogger{}
}

type noopLogger struct{}

func (noopLogger) Debug(string, ...any)        {}
func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Warn(string, ...any)         {}
func (noopLogger) Error(string, ...any)        {}
func (n noopLogger) With(...any) bellog.Logger { return n }
