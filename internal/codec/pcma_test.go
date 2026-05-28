package codec

import "testing"

// TestEncodePCMA_ReferenceVectors locks down well-known A-law points so
// regressions are caught immediately. Numbers derived from Sun's reference
// linear2alaw applied to the standard inputs.
func TestEncodePCMA_ReferenceVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pcm  int16
		alaw byte
		name string
	}{
		// A-law has no exact zero — positive zero maps to its smallest bucket.
		{0, 0xD5, "zero_positive"},
		// -1 folds (via -V-1) to magnitude 0 → same byte family but negative.
		{-1, 0x55, "minus_one_folds_to_neg_zero"},
		// 8 (smallest 13-bit-magnitude 1) still lands in segment 0 bucket 0.
		{8, 0xD5, "small_positive_8"},
		{-8, 0x55, "small_negative_8"},
		// Saturated positive: int16 max clamps into seg 7 max bucket.
		{32767, 0xAA, "max_positive_saturates"},
		// Saturated negative: int16 min clamps to seg 7 max bucket, neg sign.
		{-32768, 0x2A, "max_negative_saturates"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := EncodePCMA(c.pcm)
			if got != c.alaw {
				t.Errorf("EncodePCMA(%d) = 0x%02X, want 0x%02X", c.pcm, got, c.alaw)
			}
		})
	}
}

func TestDecodePCMA_ReferenceVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		alaw byte
		pcm  int16
		name string
	}{
		{0xD5, 8, "alaw_D5_to_8"},          // smallest positive
		{0x55, -8, "alaw_55_to_minus_8"},   // smallest negative
		{0xAA, 32256, "max_positive_alaw"}, // segment 7 max
		{0x2A, -32256, "max_negative_alaw"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := DecodePCMA(c.alaw)
			if got != c.pcm {
				t.Errorf("DecodePCMA(0x%02X) = %d, want %d", c.alaw, got, c.pcm)
			}
		})
	}
}

func TestPCMA_EncodeDecodeEncodeIdempotent(t *testing.T) {
	t.Parallel()
	for s := int32(-32768); s <= 32767; s += 7 {
		first := EncodePCMA(int16(s))
		linear := DecodePCMA(first)
		second := EncodePCMA(linear)
		if first != second {
			t.Fatalf("idempotent failed at s=%d: encode=0x%02X, "+
				"after round trip=0x%02X", s, first, second)
		}
	}
}

// TestPCMA_DecodeBijection_AllBytes ensures every distinct A-law byte decodes
// to a distinct PCM value. Unlike µ-law, A-law has no positive/negative-zero
// collision so the bijection is strict.
func TestPCMA_DecodeBijection_AllBytes(t *testing.T) {
	t.Parallel()
	seen := map[int16]byte{}
	for b := 0; b < 256; b++ {
		v := DecodePCMA(byte(b))
		if existing, dup := seen[v]; dup {
			t.Errorf("decode collision: bytes 0x%02X and 0x%02X both -> %d",
				existing, byte(b), v)
		}
		seen[v] = byte(b)
	}
	if len(seen) != 256 {
		t.Errorf("distinct decoded values = %d, want 256", len(seen))
	}
}

func TestPCMA_QuantizationErrorIsLogarithmic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		magMax int32
		tol    int32
	}{
		// A-law's smallest quantum is 16 in 16-bit units (8 in 13-bit terms
		// shifted up by 3). Plenty of headroom inside seg 0.
		{200, 32},
		// Seg 1.
		{500, 32},
		// Seg 3-ish.
		{4000, 256},
		// Approaching saturation.
		{16000, 1024},
	}
	for _, c := range cases {
		for s := -c.magMax; s <= c.magMax; s++ {
			v := DecodePCMA(EncodePCMA(int16(s)))
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

func TestPCMA_FrameHelpers(t *testing.T) {
	t.Parallel()
	src := []int16{0, 4, -4, 100, -100, 32767, -32768}
	enc := make([]byte, len(src))
	EncodePCMAFrame(enc, src)
	if enc[0] != EncodePCMA(src[0]) {
		t.Fatalf("frame[0] mismatch")
	}
	dec := make([]int16, len(enc))
	DecodePCMAFrame(dec, enc)
	for i, b := range enc {
		if dec[i] != DecodePCMA(b) {
			t.Errorf("DecodePCMAFrame[%d] mismatch", i)
		}
	}
}

func BenchmarkEncodePCMA(b *testing.B) {
	src := make([]int16, 160)
	for i := range src {
		src[i] = int16(i * 100) //nolint:gosec // bounded sweep
	}
	dst := make([]byte, len(src))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EncodePCMAFrame(dst, src)
	}
}

func BenchmarkDecodePCMA(b *testing.B) {
	src := make([]byte, 160)
	for i := range src {
		src[i] = byte(i)
	}
	dst := make([]int16, len(src))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DecodePCMAFrame(dst, src)
	}
}
