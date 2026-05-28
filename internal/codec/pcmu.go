// Package codec implements pure-Go audio codecs used by the Bellerophon SIP
// stack. The G.711 µ-law (PCMU) and A-law (PCMA) variants are bit-exact ports
// of ITU-T G.711 (11/88), validated against the Sun reference implementation.
package codec

const (
	// ulawBias is the constant added to the magnitude before segmentation,
	// per ITU-T G.711. Do not "round" — every implementation uses 0x84.
	ulawBias = 0x84
	// ulawClip is the largest magnitude representable in 14-bit signed PCM
	// before µ-law quantization. Samples beyond this saturate.
	ulawClip = 8159
)

// ulawSegEnd is the canonical Sun "seg_uend" table: each entry is the inclusive
// upper bound of µ-law segment i in the bias-adjusted magnitude domain.
var ulawSegEnd = [8]int32{0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF}

// ulawSegment performs the canonical linear search for the µ-law segment of
// a bias-adjusted magnitude. Returns 8 when the value overflows seg 7 — the
// caller handles that as the µ-law saturation case.
func ulawSegment(val int32) byte {
	for i := byte(0); i < 8; i++ {
		if val <= ulawSegEnd[i] {
			return i
		}
	}
	return 8
}

// EncodePCMU encodes a signed 16-bit PCM sample to a single G.711 µ-law byte.
// Bit-exact with Sun's reference linear2ulaw.
func EncodePCMU(sample int16) byte {
	// Drop the lowest two bits — µ-law operates on 14-bit signed magnitudes.
	s := int32(sample) >> 2

	var mask byte
	if s < 0 {
		s = -s
		mask = 0x7F
	} else {
		mask = 0xFF
	}
	if s > ulawClip {
		s = ulawClip
	}
	s += ulawBias >> 2

	seg := ulawSegment(s)
	var uval byte
	if seg >= 8 {
		// Overflow into the implicit 8th segment — saturate.
		uval = 0x7F
	} else {
		uval = (seg << 4) | byte((s>>(seg+1))&0x0F)
	}
	return uval ^ mask
}

// DecodePCMU decodes a G.711 µ-law byte to a signed 16-bit PCM sample. Mirrors
// Sun's reference ulaw2linear.
func DecodePCMU(b byte) int16 {
	b = ^b
	sign := b & 0x80
	exp := uint((b >> 4) & 0x07)
	mant := int32(b & 0x0F)
	t := (mant << 3) + ulawBias
	t <<= exp
	if sign != 0 {
		return int16(ulawBias - t) //nolint:gosec // t bounded by µ-law range; result fits int16 by construction
	}
	return int16(t - ulawBias) //nolint:gosec // same
}

// EncodePCMUFrame encodes len(src) PCM samples into dst as µ-law bytes.
// len(dst) must be ≥ len(src); the caller is responsible for sizing.
func EncodePCMUFrame(dst []byte, src []int16) {
	for i, s := range src {
		dst[i] = EncodePCMU(s)
	}
}

// DecodePCMUFrame decodes len(src) µ-law bytes into dst as PCM samples.
// len(dst) must be ≥ len(src).
func DecodePCMUFrame(dst []int16, src []byte) {
	for i, b := range src {
		dst[i] = DecodePCMU(b)
	}
}
