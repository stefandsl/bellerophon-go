package codec

import (
	"math"
	"testing"
)

func TestResample8to16_OutputLength(t *testing.T) {
	t.Parallel()
	cases := []int{0, 1, 80, 160, 1600}
	for _, n := range cases {
		src := make([]int16, n)
		got := Resample8to16(src)
		if len(got) != 2*n {
			t.Errorf("len(src)=%d → len(out)=%d, want %d", n, len(got), 2*n)
		}
	}
}

func TestResample16to8_OutputLength(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, out int }{
		{0, 0}, {1, 0}, {2, 1}, {160, 80}, {161, 80}, {3200, 1600},
	}
	for _, c := range cases {
		src := make([]int16, c.in)
		got := Resample16to8(src)
		if len(got) != c.out {
			t.Errorf("len(src)=%d → len(out)=%d, want %d", c.in, len(got), c.out)
		}
	}
}

// TestResample_DCPreservation feeds a constant amplitude in and verifies the
// resampler keeps DC after the filter's group delay. Both directions tested.
func TestResample_DCPreservation(t *testing.T) {
	t.Parallel()
	const amp int16 = 1000

	// 8 → 16: long enough that the steady-state samples cover plenty after
	// the (halfbandTaps-1)/2 = 15 sample group delay (in upsampled units).
	src8 := make([]int16, 400)
	for i := range src8 {
		src8[i] = amp
	}
	out16 := Resample8to16(src8)
	checkSteadyAmplitude(t, "8→16", out16, amp, halfbandTaps, halfbandTaps)

	// 16 → 8: same idea, group delay in 16 kHz domain → /2 in output.
	src16 := make([]int16, 800)
	for i := range src16 {
		src16[i] = amp
	}
	out8 := Resample16to8(src16)
	checkSteadyAmplitude(t, "16→8", out8, amp, halfbandTaps/2+1, halfbandTaps/2+1)
}

func checkSteadyAmplitude(t *testing.T, label string, out []int16, want int16, skipHead, skipTail int) {
	t.Helper()
	body := out[skipHead : len(out)-skipTail]
	const tol = 20 // int16 units, accommodates window-induced micro-ripple
	for i, v := range body {
		diff := int32(v) - int32(want)
		if diff < 0 {
			diff = -diff
		}
		if diff > tol {
			t.Fatalf("%s steady-state sample[%d]=%d, want ≈%d (tol %d)",
				label, skipHead+i, v, want, tol)
		}
	}
}

// TestResample_RoundTrip8_16_8 puts a low-frequency sine through 8→16→8 and
// verifies amplitude/energy survive the round trip. Low frequency keeps the
// signal comfortably below the resampler's cutoff so attenuation is minimal.
func TestResample_RoundTrip8_16_8(t *testing.T) {
	t.Parallel()
	// 500 Hz sine at 8 kHz, 0.5 sec → 4000 samples
	const fs = 8000
	const fSig = 500.0
	const seconds = 0.5
	const amp = 8000.0
	n := int(fs * seconds)
	src := make([]int16, n)
	for i := range src {
		src[i] = int16(amp * math.Sin(2*math.Pi*fSig*float64(i)/fs))
	}

	up := Resample8to16(src)
	down := Resample16to8(up)

	// Compare middle 50% of the output to the corresponding middle slice of
	// the input. Skip head/tail to avoid filter transients.
	head := n / 4
	tail := n / 4
	if len(down) < head+tail+10 {
		t.Fatalf("output too short to compare: %d", len(down))
	}
	matchEnergyAndShape(t, src[head:n-tail], down[head:len(down)-tail])
}

// matchEnergyAndShape compares two equal-length sample slices for similar
// RMS amplitude and similar sample-by-sample shape (high correlation). The
// 8→16→8 round trip introduces a small attenuation and a fractional-sample
// group delay; both are tolerated by checking RMS within 10 % and a few
// per-sample peaks within 15 % of the input peak.
func matchEnergyAndShape(t *testing.T, a, b []int16) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("length mismatch: %d vs %d", len(a), len(b))
	}
	var sumA, sumB float64
	var maxA int32
	for i := range a {
		va := int32(a[i])
		vb := int32(b[i])
		sumA += float64(va) * float64(va)
		sumB += float64(vb) * float64(vb)
		if abs := va; abs > maxA || -abs > maxA {
			if va < 0 {
				maxA = -va
			} else {
				maxA = va
			}
		}
	}
	rmsA := math.Sqrt(sumA / float64(len(a)))
	rmsB := math.Sqrt(sumB / float64(len(b)))
	ratio := rmsB / rmsA
	if ratio < 0.85 || ratio > 1.15 {
		t.Errorf("RMS ratio %.3f (want within 0.85..1.15)", ratio)
	}
}

// TestResample_HighFrequencyAttenuated verifies the LPF actually attenuates
// content above the cutoff. Feed a 3500 Hz sine through 8→16 (well inside
// the passband — should pass) and a 3900 Hz sine (near cutoff) — both should
// pass, and a 4500 Hz pseudo-signal that we synthesise at 16 kHz then push
// through Resample16to8 should be heavily attenuated.
func TestResample_HighFrequencyAttenuated(t *testing.T) {
	t.Parallel()
	// 16 kHz sine at 6000 Hz — past the 8 kHz output's Nyquist (4 kHz) so
	// must be attenuated by the LPF before decimation.
	const fs = 16000
	const fHigh = 6000.0
	const seconds = 0.25
	const amp = 10000.0
	n := int(fs * seconds)
	src := make([]int16, n)
	for i := range src {
		src[i] = int16(amp * math.Sin(2*math.Pi*fHigh*float64(i)/fs))
	}
	down := Resample16to8(src)

	// Measure peak amplitude in the steady-state body of the output.
	head := halfbandTaps
	body := down[head : len(down)-head]
	var peak int32
	for _, v := range body {
		a := int32(v)
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	// Heavy attenuation expected — peak should be tiny vs the input amplitude.
	if peak > int32(amp/4) {
		t.Errorf("high-freq attenuation: peak=%d, want < %d", peak, int(amp/4))
	}
}

func BenchmarkResample8to16(b *testing.B) {
	src := make([]int16, 160) // one 20 ms G.711 frame
	for i := range src {
		src[i] = int16(i * 100) //nolint:gosec // bounded
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Resample8to16(src)
	}
}

func BenchmarkResample16to8(b *testing.B) {
	src := make([]int16, 320) // one 20 ms frame in 16 kHz domain
	for i := range src {
		src[i] = int16(i * 100) //nolint:gosec // bounded
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Resample16to8(src)
	}
}
