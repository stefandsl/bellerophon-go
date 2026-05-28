package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stefandsl/bellerophon-go/internal/llm"
	"github.com/stefandsl/bellerophon-go/internal/vad"
)

// ===== Fakes for the four backend deps =====

type fakeSource struct{ ch chan vad.Utterance }

func newFakeSource(buf int) *fakeSource                { return &fakeSource{ch: make(chan vad.Utterance, buf)} }
func (f *fakeSource) Utterances() <-chan vad.Utterance { return f.ch }
func (f *fakeSource) push(text string) {
	// The PCM payload is opaque to STT (we mock it); a small non-nil
	// slice keeps any "empty samples" guard happy if the real
	// transcriber were swapped in.
	f.ch <- vad.Utterance{
		PCM16:      []int16{0, 1, 2, 3},
		DurationMs: 200,
		StartedAt:  time.Now(),
		EndedAt:    time.Now(),
	}
	_ = text // text is only documentation here
}

type fakeSTT struct {
	mu          sync.Mutex
	transcripts []string
	calls       atomic.Int32
	err         error
}

func (f *fakeSTT) Transcribe(ctx context.Context, _ []int16, _ int) (string, error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.transcripts) == 0 {
		return "", io.EOF
	}
	t := f.transcripts[0]
	f.transcripts = f.transcripts[1:]
	return t, nil
}

type fakeLLM struct {
	mu      sync.Mutex
	replies []string
	calls   atomic.Int32
	ended   atomic.Int32
	err     error
}

func (f *fakeLLM) Query(ctx context.Context, req llm.Request) (llm.Response, error) {
	f.calls.Add(1)
	if f.err != nil {
		return llm.Response{}, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.replies) == 0 {
		return llm.Response{Text: "default"}, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return llm.Response{Text: r, SessionID: "sess", DurationMs: 1}, nil
}

func (f *fakeLLM) EndSession(_ context.Context, _ string) error {
	f.ended.Add(1)
	return nil
}

type fakeTTS struct {
	calls atomic.Int32
	texts []string
	mu    sync.Mutex
	err   error
	delay time.Duration
}

func (f *fakeTTS) Synthesize(ctx context.Context, text, _ string) ([]byte, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
	// Returned bytes carry the text inline so tests can assert on
	// what the player received.
	return []byte("PCM:" + text), nil
}

type playRecord struct {
	text string
	took time.Duration
	err  error
}

type fakePlayer struct {
	mu           sync.Mutex
	records      []playRecord
	started      chan string   // text on each Play start
	hold         chan struct{} // when non-nil, Play blocks until closed or ctx
	playDuration time.Duration // when >0, Play sleeps that long unless ctx cancels
	err          error
}

func newFakePlayer() *fakePlayer {
	return &fakePlayer{started: make(chan string, 16)}
}
func (p *fakePlayer) Play(ctx context.Context, pcm []byte, _ int) error {
	if p.err != nil {
		return p.err
	}
	text := strings.TrimPrefix(string(pcm), "PCM:")
	start := time.Now()
	select {
	case p.started <- text:
	default:
	}
	var err error
	switch {
	case p.hold != nil:
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-p.hold:
		}
	case p.playDuration > 0:
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(p.playDuration):
		}
	}
	p.mu.Lock()
	p.records = append(p.records, playRecord{text: text, took: time.Since(start), err: err})
	p.mu.Unlock()
	return err
}
func (p *fakePlayer) playedTexts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.records))
	for _, r := range p.records {
		out = append(out, r.text)
	}
	return out
}
func (p *fakePlayer) recordFor(substr string) (playRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.records {
		if strings.Contains(r.text, substr) {
			return r, true
		}
	}
	return playRecord{}, false
}

type fakeBarge struct {
	mu    sync.Mutex
	chans []chan struct{} // history of channels; tests fire the last one
}

func newFakeBarge() *fakeBarge { return &fakeBarge{} }
func (b *fakeBarge) Speech() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{})
	b.chans = append(b.chans, ch)
	return ch
}
func (b *fakeBarge) Reset() {}
func (b *fakeBarge) fire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.chans) == 0 {
		return
	}
	close(b.chans[len(b.chans)-1])
}

// ===== Loop tests =====

func defaultOpts(src *fakeSource, sttp *fakeSTT, lp *fakeLLM, tts *fakeTTS, pl *fakePlayer) Options {
	return Options{
		CallID:           "test-call",
		VAD:              src,
		STT:              sttp,
		LLM:              lp,
		TTS:              tts,
		Player:           pl,
		UtteranceTimeout: 50 * time.Millisecond, // fast tests
		LLMTimeout:       2 * time.Second,
		TTSTimeout:       2 * time.Second,
	}
}

func TestNew_RequiresEveryDependency(t *testing.T) {
	t.Parallel()
	base := defaultOpts(newFakeSource(1), &fakeSTT{}, &fakeLLM{}, &fakeTTS{}, newFakePlayer())
	cases := map[string]func(*Options){
		"CallID": func(o *Options) { o.CallID = "" },
		"VAD":    func(o *Options) { o.VAD = nil },
		"STT":    func(o *Options) { o.STT = nil },
		"LLM":    func(o *Options) { o.LLM = nil },
		"TTS":    func(o *Options) { o.TTS = nil },
		"Player": func(o *Options) { o.Player = nil },
	}
	for name, mut := range cases {
		opts := base
		mut(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("missing %s: expected error", name)
		}
	}
}

func TestLoop_HappyPath_GreetThenTurnThenGoodbye(t *testing.T) {
	t.Parallel()
	src := newFakeSource(4)
	sttp := &fakeSTT{transcripts: []string{"what is the time", "goodbye"}}
	lp := &fakeLLM{replies: []string{"it is noon"}}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	opts := defaultOpts(src, sttp, lp, tts, pl)

	loop, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	src.push("turn 1")
	src.push("turn 2 (goodbye)")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := loop.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	played := pl.playedTexts()
	want := []string{
		"Hello! I'm Aïtheia. How can I help you today?", // greeting
		"it is noon",                   // LLM reply turn 1
		"Goodbye! Call again anytime.", // farewell after goodbye keyword
	}
	if len(played) != len(want) {
		t.Fatalf("played count = %d, want %d (%v)", len(played), len(want), played)
	}
	for i, w := range want {
		if played[i] != w {
			t.Errorf("played[%d] = %q, want %q", i, played[i], w)
		}
	}
	if got := lp.ended.Load(); got != 1 {
		t.Errorf("EndSession calls = %d, want 1", got)
	}
	if loop.State() != StateHangup {
		t.Errorf("final state = %v, want HANGUP", loop.State())
	}
}

func TestLoop_NoSpeechTimeout_PromptsAndRetries(t *testing.T) {
	t.Parallel()
	src := newFakeSource(2)
	sttp := &fakeSTT{transcripts: []string{"hi", "bye"}}
	lp := &fakeLLM{replies: []string{"hello back"}}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	opts := defaultOpts(src, sttp, lp, tts, pl)
	opts.UtteranceTimeout = 30 * time.Millisecond
	loop, _ := New(opts)

	// Push the first utterance late (after the timeout).
	go func() {
		time.Sleep(80 * time.Millisecond) // ≥ 2 timeouts so at least one fires
		src.push("late")
		time.Sleep(20 * time.Millisecond)
		src.push("bye")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = loop.Run(ctx)

	played := pl.playedTexts()
	// Must contain at least one NoSpeech prompt (the canned phrase).
	found := false
	for _, p := range played {
		if p == "I didn't hear anything. Are you still there?" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected NoSpeech prompt in playback, got %v", played)
	}
}

func TestLoop_ShortTranscriptTriggersClarify(t *testing.T) {
	t.Parallel()
	src := newFakeSource(2)
	sttp := &fakeSTT{transcripts: []string{"", "goodbye"}}
	lp := &fakeLLM{}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	loop, _ := New(defaultOpts(src, sttp, lp, tts, pl))

	src.push("noise")
	src.push("goodbye")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = loop.Run(ctx)

	played := pl.playedTexts()
	wantClarify := false
	for _, p := range played {
		if p == "Sorry, I didn't catch that. Could you repeat?" {
			wantClarify = true
		}
	}
	if !wantClarify {
		t.Errorf("expected Clarify prompt, got %v", played)
	}
}

func TestLoop_LLMErrorFallsBackToBridgeUnknown(t *testing.T) {
	t.Parallel()
	src := newFakeSource(2)
	sttp := &fakeSTT{transcripts: []string{"how does X work", "bye"}}
	lp := &fakeLLM{err: errors.New("upstream down")}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	loop, _ := New(defaultOpts(src, sttp, lp, tts, pl))

	src.push("ask")
	src.push("bye")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = loop.Run(ctx)

	played := pl.playedTexts()
	if !contains(played, "Sorry, I ran into an unexpected error. One moment, please.") {
		t.Errorf("expected BridgeUnknown fallback, got %v", played)
	}
}

func TestLoop_MaxTurnsTerminates(t *testing.T) {
	t.Parallel()
	src := newFakeSource(8)
	sttp := &fakeSTT{transcripts: []string{"aaa", "bbb", "ccc"}}
	lp := &fakeLLM{replies: []string{"r1", "r2", "r3"}}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	opts := defaultOpts(src, sttp, lp, tts, pl)
	opts.MaxTurns = 2
	loop, _ := New(opts)

	for i := 0; i < 4; i++ {
		src.push("turn")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = loop.Run(ctx)

	if !contains(pl.playedTexts(), "We've been talking for a while. Goodbye!") {
		t.Errorf("expected MaxTurns farewell, got %v", pl.playedTexts())
	}
	if loop.Turn() != 2 {
		t.Errorf("turn = %d, want 2 (capped by MaxTurns)", loop.Turn())
	}
}

func TestLoop_TranscriptJSONLPersisted(t *testing.T) {
	t.Parallel()
	src := newFakeSource(2)
	sttp := &fakeSTT{transcripts: []string{"hello there", "goodbye"}}
	lp := &fakeLLM{replies: []string{"hi friend"}}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	opts := defaultOpts(src, sttp, lp, tts, pl)
	opts.TranscriptDir = t.TempDir()
	opts.CallID = "call-trans"
	loop, _ := New(opts)

	src.push("u1")
	src.push("u2")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = loop.Run(ctx)

	path := filepath.Join(opts.TranscriptDir, "call-trans.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("transcript missing: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var turns []Turn
	for sc.Scan() {
		var tn Turn
		if err := json.Unmarshal(sc.Bytes(), &tn); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		turns = append(turns, tn)
	}
	if len(turns) < 3 {
		t.Fatalf("turns = %d, want >=3 (user+assistant+user)", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Text != "hello there" || turns[0].Turn != 1 {
		t.Errorf("turn[0] = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "hi friend" {
		t.Errorf("turn[1] = %+v", turns[1])
	}
}

// TestLoop_BargeIn_AbortsPlayback drives the loop to SPEAKING, fires
// the barge-in channel mid-playback, and verifies the player was
// cancelled and the loop returned to LISTENING ready for the next
// utterance.
func TestLoop_BargeIn_AbortsPlayback(t *testing.T) {
	t.Parallel()
	src := newFakeSource(2)
	sttp := &fakeSTT{transcripts: []string{"a question", "goodbye"}}
	lp := &fakeLLM{replies: []string{"this is a long answer the user will interrupt"}}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	// Every Play takes 1 s unless cancelled; gives the test a wide
	// window to fire barge-in during the LLM-reply playback.
	pl.playDuration = 1 * time.Second
	barge := newFakeBarge()

	opts := defaultOpts(src, sttp, lp, tts, pl)
	opts.BargeIn = barge
	loop, _ := New(opts)

	src.push("q1")

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = loop.Run(ctx)
		close(done)
	}()

	// Wait until the LLM reply has started playing. The Play before
	// it is the greeting — we ignore that.
	deadline := time.Now().Add(3 * time.Second)
	var sawLLMReply bool
	for time.Now().Before(deadline) {
		select {
		case text := <-pl.started:
			if strings.Contains(text, "this is a long answer") {
				sawLLMReply = true
			}
		case <-time.After(20 * time.Millisecond):
		}
		if sawLLMReply {
			break
		}
	}
	if !sawLLMReply {
		t.Fatal("LLM-reply Play never started")
	}

	// Trip barge-in mid-playback.
	barge.fire()

	// Push the goodbye so the loop can terminate cleanly after
	// returning to LISTENING.
	src.push("bye")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not return after barge-in + goodbye")
	}

	// The salient check: the LLM-reply Play call returned with
	// context.Canceled and took much less than its full 1 s duration.
	// This is the M002 §2 barge-in latency guarantee — speech onset
	// must cancel SPEAKING quickly. We allow 200 ms of slack for
	// channel close + goroutine scheduling under the race detector.
	rec, ok := pl.recordFor("this is a long answer")
	if !ok {
		t.Fatal("LLM-reply Play record missing")
	}
	if !errors.Is(rec.err, context.Canceled) {
		t.Errorf("LLM-reply Play err = %v, want context.Canceled", rec.err)
	}
	if rec.took > 200*time.Millisecond {
		t.Errorf("barge-in did not cancel quickly: Play took %v", rec.took)
	}
}

func TestLoop_TTSFailureSkipsPlay(t *testing.T) {
	t.Parallel()
	src := newFakeSource(2)
	sttp := &fakeSTT{transcripts: []string{"question", "bye"}}
	lp := &fakeLLM{replies: []string{"answer"}}
	tts := &fakeTTS{err: errors.New("rate-limited")}
	pl := newFakePlayer()
	loop, _ := New(defaultOpts(src, sttp, lp, tts, pl))

	src.push("q")
	src.push("bye")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = loop.Run(ctx)

	// All TTS calls failed → player should have received nothing.
	if got := len(pl.records); got != 0 {
		t.Errorf("player calls = %d, want 0 (TTS error path)", got)
	}
}

func TestLoop_CtxCancelMidFlight(t *testing.T) {
	t.Parallel()
	src := newFakeSource(2)
	sttp := &fakeSTT{transcripts: []string{"q"}}
	lp := &fakeLLM{replies: []string{"a"}}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	loop, _ := New(defaultOpts(src, sttp, lp, tts, pl))

	src.push("q")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := loop.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run returned unexpected error: %v", err)
	}
	if got := lp.ended.Load(); got != 1 {
		t.Errorf("EndSession not called on ctx-cancel cleanup: got %d", got)
	}
}

// TestLoop_SourceCloseDrivesToHangup confirms the loop terminates
// cleanly when the audio source channel closes (call dropped).
func TestLoop_SourceCloseDrivesToHangup(t *testing.T) {
	t.Parallel()
	src := newFakeSource(0)
	sttp := &fakeSTT{}
	lp := &fakeLLM{}
	tts := &fakeTTS{}
	pl := newFakePlayer()
	loop, _ := New(defaultOpts(src, sttp, lp, tts, pl))
	close(src.ch)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := loop.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if loop.State() != StateHangup {
		t.Errorf("state = %v, want HANGUP", loop.State())
	}
}

func TestStateString(t *testing.T) {
	t.Parallel()
	for s, want := range map[State]string{
		StateIdle:         "IDLE",
		StateListening:    "LISTENING",
		StateTranscribing: "TRANSCRIBING",
		StateThinking:     "THINKING",
		StateSpeaking:     "SPEAKING",
		StateHangup:       "HANGUP",
	} {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(s), got, want)
		}
	}
	if got := State(99).String(); !strings.HasPrefix(got, "State(") {
		t.Errorf("unknown state String = %q", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
