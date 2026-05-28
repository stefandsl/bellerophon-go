package integration_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// requireAudioFixture returns an absolute path to a 16 kHz mono PCM
// file with a known phrase. The path is read from the named env var.
// Without one the test is skipped — there is no in-repo fixture
// because it would have to be a real human voice for Whisper to
// transcribe, and shipping audio with the source tree is something
// we'd rather defer to a follow-up.
//
// Suggested fixtures the operator records once and reuses:
//
//	hello.pcm        — "Hello, how are you today?"
//	goodbye.pcm      — "Goodbye, thanks for your help."
//	question.pcm     — "What's the weather like in Rome right now?"
//
// All mono PCM16 LE at 16 kHz (Whisper-native). `sox in.wav -r 16000
// -b 16 -c 1 out.pcm` does the conversion.
func requireAudioFixture(t *testing.T, envVar string) string {
	t.Helper()
	p := os.Getenv(envVar)
	if p == "" {
		t.Skipf("skipped: set %s to the path of a mono PCM16 16 kHz fixture", envVar)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs(%q): %v", p, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("audio fixture %q not readable: %v", abs, err)
	}
	return abs
}

// loadPCM16 reads a raw mono PCM16 LE file into an int16 slice.
// No header parsing — the bytes ARE the samples.
func loadPCM16(t *testing.T, path string) []int16 {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test helper; path comes from env
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if len(data)%2 != 0 {
		t.Fatalf("pcm16 length must be even, got %d", len(data))
	}
	out := make([]int16, len(data)/2)
	for i := range out {
		out[i] = int16(data[2*i]) | int16(data[2*i+1])<<8 //nolint:gosec // intentional LE decode
	}
	return out
}

// latencyStats computes P50/P95/P99 from a copy of durations. The
// caller passes the durations slice; we sort our own copy so the
// caller's slice isn't disturbed.
type latencyStats struct {
	N      int
	Min    time.Duration
	P50    time.Duration
	P95    time.Duration
	P99    time.Duration
	Max    time.Duration
	MeanMs float64
}

func computeStats(samples []time.Duration) latencyStats {
	if len(samples) == 0 {
		return latencyStats{}
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, s := range samples {
		total += s
	}
	pct := func(p float64) time.Duration {
		// nearest-rank percentile: index = ceil(p * n) - 1
		idx := int(float64(len(sorted)-1) * p)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	return latencyStats{
		N:      len(sorted),
		Min:    sorted[0],
		P50:    pct(0.50),
		P95:    pct(0.95),
		P99:    pct(0.99),
		Max:    sorted[len(sorted)-1],
		MeanMs: float64(total) / float64(len(sorted)) / float64(time.Millisecond),
	}
}
