package codec

import (
	"testing"
)

// TestEncodePCMU_ReferenceVectors locks down a handful of known points so a
// future refactor cannot silently change the bit-exact output.
func TestEncodePCMU_ReferenceVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pcm  int16
		ulaw byte
		name string
	}{
		// Positive zero — every µ-law impl encodes 0 → 0xFF.
		{0, 0xFF, "zero"},
		// Saturated positive: max int16 clamps into µ-law segment 7 max.
		{32767, 0x80, "max_positive_saturates"},
		// Saturated negative: min int16 clamps to segment 7 max with sign bit.
		{-32768, 0x00, "max_negative_saturates"},
		// Small positive: 4 maps just into seg 0 (after >>2 it's 1, +bias).
		{4, 0xFE, "small_positive_1"},
		// Small negative: -4 should mirror 4 with sign bit.
		{-4, 0x7E, "small_negative_1"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := EncodePCMU(c.pcm)
			if got != c.ulaw {
				t.Errorf("EncodePCMU(%d) = 0x%02X, want 0x%02X", c.pcm, got, c.ulaw)
			}
		})
	}
}

// TestDecodePCMU_ReferenceVectors mirrors the encode reference table. Decode
// is well-defined for every byte; we check the symmetric points.
func TestDecodePCMU_ReferenceVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ulaw byte
		pcm  int16
		name string
	}{
		{0xFF, 0, "ulaw_FF_to_zero"},
		// Max positive µ-law byte → the saturated decoded value (Sun reference).
		{0x80, 32124, "max_positive_ulaw"},
		{0x00, -32124, "max_negative_ulaw"},
		{0x7F, 0, "sign_negative_zero"},
		{0xFE, 8, "ulaw_FE_to_8"},
		{0x7E, -8, "ulaw_7E_to_minus_8"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := DecodePCMU(c.ulaw)
			if got != c.pcm {
				t.Errorf("DecodePCMU(0x%02X) = %d, want %d", c.ulaw, got, c.pcm)
			}
		})
	}
}

// TestPCMU_EncodeDecodeEncodeIdempotent verifies that encoding a linear
// sample, decoding back, then re-encoding produces the original µ-law byte.
// This is the well-defined fixed-point property of any lossy codec:
// encode∘decode∘encode = encode.
func TestPCMU_EncodeDecodeEncodeIdempotent(t *testing.T) {
	t.Parallel()
	// Sweep every 7th sample in the int16 range — full sweep would be slow
	// for no extra coverage; the codec is piecewise-linear with monotonic
	// behaviour inside each segment, so a dense-but-not-exhaustive sweep is
	// sufficient to catch any segment-boundary regression.
	for s := int32(-32768); s <= 32767; s += 7 {
		first := EncodePCMU(int16(s))
		linear := DecodePCMU(first)
		second := EncodePCMU(linear)
		if first != second {
			t.Fatalf("idempotent failed at s=%d: encode=0x%02X, "+
				"after round trip=0x%02X", s, first, second)
		}
	}
}

// TestPCMU_DecodeBijection_AllBytes ensures every distinct µ-law byte decodes
// to a distinct PCM value (modulo the negative-zero / positive-zero pair
// which collapse to 0 by construction).
func TestPCMU_DecodeBijection_AllBytes(t *testing.T) {
	t.Parallel()
	seen := map[int16]byte{}
	for b := 0; b < 256; b++ {
		v := DecodePCMU(byte(b))
		if existing, dup := seen[v]; dup {
			// Negative-zero / positive-zero collision is the one allowed pair.
			if v == 0 {
				continue
			}
			t.Errorf("decode collision: bytes 0x%02X and 0x%02X both -> %d",
				existing, byte(b), v)
		}
		seen[v] = byte(b)
	}
}

// TestPCMU_QuantizationErrorIsLogarithmic verifies that the round-trip
// quantization error stays inside µ-law's per-segment bucket width: ~32 in
// 16-bit units inside seg 0 and ~2*previous as the magnitude doubles. This
// is the closest analogue to "1 LSB" for a logarithmically-spaced codec.
func TestPCMU_QuantizationErrorIsLogarithmic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		magMax int32 // sweep |s| up to this
		tol    int32 // max permitted |decode(encode(s)) - s| in 16-bit units
	}{
		// Comfortably inside segment 0 (away from the seg0/seg1 boundary).
		{500, 32},
		// Inside segment 1 — bucket roughly doubles.
		{1500, 64},
		// Segment 3-ish.
		{4000, 256},
		// Heading into the high segments — still bounded.
		{16000, 1024},
	}
	for _, c := range cases {
		for s := -c.magMax; s <= c.magMax; s++ {
			v := DecodePCMU(EncodePCMU(int16(s)))
			err := int32(v) - s
			if err < 0 {
				err = -err
			}
			if err > c.tol {
				t.Fatalf("|s|≤%d round trip at %d: got %d (error %d > tol %d)",
					c.magMax, s, v, err, c.tol)
			}
		}
	}
}

// TestPCMU_FrameHelpers exercises the slice wrappers.
func TestPCMU_FrameHelpers(t *testing.T) {
	t.Parallel()
	src := []int16{0, 4, -4, 100, -100, 32767, -32768}
	enc := make([]byte, len(src))
	EncodePCMUFrame(enc, src)

	expectedFirst := EncodePCMU(src[0])
	if enc[0] != expectedFirst {
		t.Fatalf("EncodePCMUFrame[0] = 0x%02X, want 0x%02X", enc[0], expectedFirst)
	}
	dec := make([]int16, len(enc))
	DecodePCMUFrame(dec, enc)
	for i := range src {
		want := DecodePCMU(enc[i])
		if dec[i] != want {
			t.Errorf("DecodePCMUFrame[%d] = %d, want %d", i, dec[i], want)
		}
	}
}

func BenchmarkEncodePCMU(b *testing.B) {
	src := make([]int16, 160) // one 20 ms G.711 frame at 8 kHz
	for i := range src {
		src[i] = int16(i * 100) //nolint:gosec // bounded sweep
	}
	dst := make([]byte, len(src))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EncodePCMUFrame(dst, src)
	}
}

func BenchmarkDecodePCMU(b *testing.B) {
	src := make([]byte, 160)
	for i := range src {
		src[i] = byte(i)
	}
	dst := make([]int16, len(src))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DecodePCMUFrame(dst, src)
	}
}
