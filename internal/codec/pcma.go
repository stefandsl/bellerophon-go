package codec

// G.711 A-law (PCMA, payload type 8) per ITU-T G.711 (11/88). Pure-Go,
// bit-exact with Sun's reference linear2alaw / alaw2linear.

// alawSegEnd is the per-segment upper-bound table for A-law search, in the
// 13-bit-magnitude domain after the sign-fold preprocessing.
var alawSegEnd = [8]int32{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

// alawSegment performs the canonical linear search for the A-law segment of
// a 13-bit magnitude. Returns 8 on overflow (saturation case).
func alawSegment(val int32) byte {
	for i := byte(0); i < 8; i++ {
		if val <= alawSegEnd[i] {
			return i
		}
	}
	return 8
}

// EncodePCMA encodes a signed 16-bit PCM sample to a single G.711 A-law byte.
// Bit-exact with Sun's reference linear2alaw, including the odd-bit-inversion
// (XOR 0x55) line-coding mask.
func EncodePCMA(sample int16) byte {
	// A-law operates on 13-bit signed magnitudes: drop the low 3 bits.
	pcm := int32(sample) >> 3

	var mask byte
	if pcm >= 0 {
		mask = 0xD5 // 0x55 ^ 0x80 — sign bit pre-applied for positives
	} else {
		mask = 0x55
		pcm = -pcm - 1 // A-law's "folded" negative magnitude
	}

	seg := alawSegment(pcm)
	if seg >= 8 {
		// Overflow into the implicit 8th segment — saturate.
		return 0x7F ^ mask
	}
	aval := seg << 4
	if seg < 2 {
		aval |= byte((pcm >> 1) & 0x0F)
	} else {
		aval |= byte((pcm >> seg) & 0x0F)
	}
	return aval ^ mask
}

// DecodePCMA decodes a G.711 A-law byte to a signed 16-bit PCM sample.
// Note the inverted sign convention vs. µ-law: after the 0x55 XOR strips the
// line-coding mask, sign-bit set means the original sample was POSITIVE.
func DecodePCMA(b byte) int16 {
	b ^= 0x55
	seg := uint((b & 0x70) >> 4)
	mant := int32(b&0x0F) << 4

	var t int32
	switch seg {
	case 0:
		t = mant + 8
	case 1:
		t = mant + 0x108
	default:
		t = (mant + 0x108) << (seg - 1)
	}
	if b&0x80 != 0 {
		return int16(t) //nolint:gosec // t bounded by A-law range; max 32256 fits int16
	}
	return int16(-t) //nolint:gosec // same; -32256 fits int16
}

// EncodePCMAFrame encodes len(src) PCM samples into dst as A-law bytes.
// len(dst) must be ≥ len(src).
func EncodePCMAFrame(dst []byte, src []int16) {
	for i, s := range src {
		dst[i] = EncodePCMA(s)
	}
}

// DecodePCMAFrame decodes len(src) A-law bytes into dst as PCM samples.
// len(dst) must be ≥ len(src).
func DecodePCMAFrame(dst []int16, src []byte) {
	for i, b := range src {
		dst[i] = DecodePCMA(b)
	}
}
