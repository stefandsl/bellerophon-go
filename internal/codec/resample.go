package codec

import "math"

// halfbandTaps is the number of FIR taps used by the 1:2 / 2:1 resampler.
// Odd, ~31, gives good narrowband telephony performance (~40 dB stopband
// with the Hamming window below) and keeps the group delay (taps-1)/2 short.
const halfbandTaps = 31

// halfbandLPF is a Hamming-windowed sinc lowpass filter with cutoff at
// fs_in/4 (i.e. 4 kHz when running on the 16 kHz domain inside the
// resamplers). Computed at package init so we don't carry a precomputed
// coefficient table that future audits would need to re-derive.
var halfbandLPF [halfbandTaps]float32

func init() {
	const n = halfbandTaps
	const center = (n - 1) / 2
	const cutoff = 0.25 // normalized to upsampled-domain Nyquist
	var sum float64
	for i := 0; i < n; i++ {
		var h float64
		if i == center {
			h = 2 * cutoff
		} else {
			x := math.Pi * float64(i-center)
			h = math.Sin(2*math.Pi*cutoff*float64(i-center)) / x
		}
		// Hamming window — simpler than Kaiser, no Bessel needed, and
		// 40 dB stopband is fine for 8↔16 kHz telephony resampling.
		w := 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(n-1))
		h *= w
		halfbandLPF[i] = float32(h)
		sum += h
	}
	// Normalize for unity DC gain.
	for i := range halfbandLPF {
		halfbandLPF[i] = float32(float64(halfbandLPF[i]) / sum)
	}
}

// Resample8to16 upsamples PCM16 audio from 8 kHz to 16 kHz by 1:2 zero
// insertion followed by the halfband LPF. The returned slice has length
// 2 * len(src). A group delay of (halfbandTaps-1)/2 samples in the
// upsampled domain is inherent; the first ~15 output samples are transient.
//
// This is a single-shot (non-streaming) helper — every call assumes silence
// outside the input window. For streaming use, keep a tail-state buffer of
// halfbandTaps-1 samples and prepend it on the next call.
func Resample8to16(src []int16) []int16 {
	const n = halfbandTaps
	out := make([]int16, 2*len(src))
	for outIdx := range out {
		var acc float32
		for k := 0; k < n; k++ {
			// Zero-inserted source: x_up[m] = src[m/2] for even m, 0 for odd.
			m := outIdx - k
			if m < 0 {
				continue
			}
			if m%2 != 0 {
				continue
			}
			srcIdx := m / 2
			if srcIdx >= len(src) {
				continue
			}
			acc += halfbandLPF[k] * float32(src[srcIdx])
		}
		// Multiply by 2 to compensate for the zero-insertion DC loss.
		out[outIdx] = clipInt16(2 * acc)
	}
	return out
}

// Resample16to8 downsamples PCM16 audio from 16 kHz to 8 kHz by applying the
// halfband LPF and decimating by 2. The returned slice has length
// len(src) / 2 (integer division — a trailing odd sample is discarded).
// Group delay (halfbandTaps-1)/2 / 2 samples in the output domain.
func Resample16to8(src []int16) []int16 {
	const n = halfbandTaps
	out := make([]int16, len(src)/2)
	for outIdx := range out {
		var acc float32
		for k := 0; k < n; k++ {
			m := 2*outIdx - k
			if m < 0 || m >= len(src) {
				continue
			}
			acc += halfbandLPF[k] * float32(src[m])
		}
		out[outIdx] = clipInt16(acc)
	}
	return out
}

// clipInt16 saturates a float32 sample into the int16 range. Used by the
// resamplers to avoid wrap-around on the rare overshoot from filter ringing.
func clipInt16(v float32) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	default:
		return int16(v)
	}
}
