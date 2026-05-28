package conversation

import (
	"testing"
	"time"
)

// pcmTone returns ms milliseconds of a constant-amplitude waveform at
// the given sample rate. Constant amplitude is fine because the
// detector only inspects RMS / peak — pitch is irrelevant.
func pcmTone(ms int, sampleRate int, amplitude int16) []int16 {
	n := sampleRate * ms / 1000
	out := make([]int16, n)
	for i := range out {
		out[i] = amplitude
	}
	return out
}

func TestEnergyBarge_FiresOnSustainedEnergy(t *testing.T) {
	t.Parallel()
	b := NewEnergyBarge(EnergyBargeOptions{})
	b.Reset()
	ch := b.Speech()

	// Push 100 ms of loud audio — well over the 60 ms MinOnsetMs.
	b.Push(pcmTone(100, 16000, 12000))

	select {
	case <-ch:
		// expected
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Speech() did not fire after sustained energy")
	}
}

func TestEnergyBarge_IgnoresSilence(t *testing.T) {
	t.Parallel()
	b := NewEnergyBarge(EnergyBargeOptions{})
	b.Reset()
	ch := b.Speech()

	// 300 ms of silence (zero samples).
	b.Push(pcmTone(300, 16000, 0))

	select {
	case <-ch:
		t.Fatal("Speech() fired on pure silence")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestEnergyBarge_RequiresContinuousOnset(t *testing.T) {
	t.Parallel()
	// Each chunk is 30 ms — below MinOnsetMs=60. Two consecutive
	// noise chunks SHOULD trip; alternating noise+silence should NOT.
	b := NewEnergyBarge(EnergyBargeOptions{MinOnsetMs: 60})
	b.Reset()

	// 30 ms noise, 30 ms silence, 30 ms noise → onset window
	// resets on the silence, so the second noise alone (30 ms) is
	// below threshold.
	b.Push(pcmTone(30, 16000, 12000))
	b.Push(pcmTone(30, 16000, 0))
	b.Push(pcmTone(30, 16000, 12000))

	select {
	case <-b.Speech():
		t.Fatal("Speech() fired on broken onset (should require continuous)")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestEnergyBarge_DisarmedByDefault(t *testing.T) {
	t.Parallel()
	b := NewEnergyBarge(EnergyBargeOptions{})
	// No Reset() called — pushing audio should not trip.
	b.Push(pcmTone(200, 16000, 16000))
	select {
	case <-b.Speech():
		t.Fatal("Speech() fired without Reset(); detector should start disarmed")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestEnergyBarge_ResetRearms(t *testing.T) {
	t.Parallel()
	b := NewEnergyBarge(EnergyBargeOptions{})
	b.Reset()
	b.Push(pcmTone(100, 16000, 12000))
	<-b.Speech() // confirm first arming fired

	// Reset and verify a fresh Speech() channel is unfired.
	b.Reset()
	freshCh := b.Speech()
	select {
	case <-freshCh:
		t.Fatal("Fresh Speech() channel was already fired after Reset()")
	case <-time.After(20 * time.Millisecond):
	}

	// Push more loud audio → should fire on the fresh channel.
	b.Push(pcmTone(100, 16000, 12000))
	select {
	case <-freshCh:
		// expected
	case <-time.After(20 * time.Millisecond):
		t.Fatal("Speech() did not re-fire after Reset()")
	}
}

func TestEnergyBarge_PeakOnsetTrigger(t *testing.T) {
	t.Parallel()
	// Single spike that exceeds MaxThreshold but not RMS (because it
	// is brief). With MinOnsetMs=10 and a 20 ms chunk, peak should
	// trip.
	b := NewEnergyBarge(EnergyBargeOptions{
		RMSThreshold: 30000, // very high RMS bar
		MaxThreshold: 5000,  // easy peak bar
		MinOnsetMs:   10,
	})
	b.Reset()
	samples := make([]int16, 320) // 20 ms at 16 kHz
	samples[100] = 20000          // one big spike
	b.Push(samples)
	select {
	case <-b.Speech():
		// expected — peak-based onset trips even when RMS is low.
	case <-time.After(20 * time.Millisecond):
		t.Fatal("peak-onset spike did not fire")
	}
}

func TestEnergyBarge_LatencyUnder100ms(t *testing.T) {
	t.Parallel()
	// Validates M002 §2: barge-in must abort playback within 100 ms
	// of caller speech detected. The detector itself trips at
	// MinOnsetMs (default 60); we add a generous slack for the channel
	// close + the caller's cancel() propagation.
	b := NewEnergyBarge(EnergyBargeOptions{})
	b.Reset()
	ch := b.Speech()

	start := time.Now()
	// Push audio in 20 ms increments — realistic frame size.
	for i := 0; i < 10; i++ {
		b.Push(pcmTone(20, 16000, 12000))
	}
	select {
	case <-ch:
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("barge-in detection took %v, want <= 100 ms", elapsed)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("barge-in did not fire within 100 ms window")
	}
}

func TestOnsetDeadline(t *testing.T) {
	t.Parallel()
	if got := onsetDeadline(16000, 16000); got != time.Second {
		t.Errorf("onsetDeadline(16000, 16000) = %v, want 1s", got)
	}
	if got := onsetDeadline(0, 0); got != 0 {
		t.Errorf("onsetDeadline(0, 0) = %v, want 0", got)
	}
}
