package media

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRecorder_BasicRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "call.wav")

	rec, err := NewRecorder(RecorderOptions{Path: path})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	inbound := []int16{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	outbound := []int16{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000}
	rec.WriteInbound(inbound)
	rec.WriteOutbound(outbound)

	if err := rec.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Final file should exist, .partial should not.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	if _, err := os.Stat(path + ".partial"); err == nil {
		t.Error(".partial still present after Stop — atomic rename failed")
	}

	// Read it back and verify mix.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	wav, err := ReadWAV(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if wav.SampleRate != DefaultRecordingSampleRate {
		t.Errorf("sample rate %d, want %d", wav.SampleRate, DefaultRecordingSampleRate)
	}
	if wav.Channels != 1 {
		t.Errorf("channels %d, want 1", wav.Channels)
	}
	if len(wav.Samples) != len(inbound) {
		t.Fatalf("samples %d, want %d", len(wav.Samples), len(inbound))
	}
	for i := range inbound {
		want := int16(int32(inbound[i]) + int32(outbound[i]))
		if wav.Samples[i] != want {
			t.Errorf("sample[%d] = %d, want %d (in=%d + out=%d)",
				i, wav.Samples[i], want, inbound[i], outbound[i])
		}
	}
}

func TestRecorder_MixSaturates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec, err := NewRecorder(RecorderOptions{Path: filepath.Join(dir, "sat.wav")})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	// Two streams whose sum overflows int16 in both directions.
	rec.WriteInbound([]int16{30000, -30000, 0})
	rec.WriteOutbound([]int16{10000, -10000, 0})
	if err := rec.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "sat.wav"))
	wav, err := ReadWAV(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	want := []int16{32767, -32768, 0}
	for i, s := range want {
		if wav.Samples[i] != s {
			t.Errorf("sample[%d] = %d, want %d (saturation)", i, wav.Samples[i], s)
		}
	}
}

func TestRecorder_UnevenStreamsPadWithZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec, _ := NewRecorder(RecorderOptions{Path: filepath.Join(dir, "uneven.wav")})

	rec.WriteInbound([]int16{1, 2, 3, 4, 5})
	rec.WriteOutbound([]int16{10, 20}) // shorter
	if err := rec.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "uneven.wav"))
	wav, _ := ReadWAV(bytes.NewReader(raw))
	want := []int16{11, 22, 3, 4, 5} // outbound pads with 0 after index 1
	if len(wav.Samples) != len(want) {
		t.Fatalf("len = %d, want %d", len(wav.Samples), len(want))
	}
	for i, s := range want {
		if wav.Samples[i] != s {
			t.Errorf("sample[%d] = %d, want %d", i, wav.Samples[i], s)
		}
	}
}

func TestRecorder_StopIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec, _ := NewRecorder(RecorderOptions{Path: filepath.Join(dir, "idem.wav")})
	rec.WriteInbound([]int16{1})
	if err := rec.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := rec.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestRecorder_CancelRemovesPartial(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cancelled.wav")
	rec, _ := NewRecorder(RecorderOptions{Path: path})

	// Manually create a .partial to simulate a prior crash.
	if err := os.WriteFile(path+".partial", []byte("junk"), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}

	rec.WriteInbound([]int16{1, 2, 3})
	if err := rec.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := os.Stat(path + ".partial"); err == nil {
		t.Error(".partial still present after Cancel")
	}
	// Stop after cancel must error.
	if err := rec.Stop(); err == nil {
		t.Error("Stop after Cancel should error")
	}
	// Writes after cancel are no-ops (no panic, no later effect).
	rec.WriteInbound([]int16{99})
}

func TestRecorder_WritesAfterStopAreNoops(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec, _ := NewRecorder(RecorderOptions{Path: filepath.Join(dir, "post-stop.wav")})
	rec.WriteInbound([]int16{1, 2, 3})
	_ = rec.Stop()
	rec.WriteInbound([]int16{4, 5, 6}) // must not panic, must not affect file
	raw, _ := os.ReadFile(filepath.Join(dir, "post-stop.wav"))
	wav, _ := ReadWAV(bytes.NewReader(raw))
	if len(wav.Samples) != 3 {
		t.Errorf("post-Stop write leaked: got %d samples, want 3", len(wav.Samples))
	}
}

func TestRecorder_ConcurrentWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec, _ := NewRecorder(RecorderOptions{Path: filepath.Join(dir, "concurrent.wav")})

	const writes = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			rec.WriteInbound([]int16{int16(i)}) //nolint:gosec // bounded
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			rec.WriteOutbound([]int16{int16(-i)}) //nolint:gosec // bounded
		}
	}()
	wg.Wait()
	if err := rec.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "concurrent.wav"))
	wav, _ := ReadWAV(bytes.NewReader(raw))
	if len(wav.Samples) != writes {
		t.Errorf("len = %d, want %d", len(wav.Samples), writes)
	}
	// The exact mixed value at each index depends on interleaving — only
	// the absence of races (caught by `go test -race`) and the final
	// length are asserted here.
}

func TestNewRecorder_RejectsEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := NewRecorder(RecorderOptions{}); err == nil {
		t.Error("NewRecorder with empty Path: want error")
	}
}

func TestRecorder_CustomSampleRate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec, _ := NewRecorder(RecorderOptions{
		Path:       filepath.Join(dir, "8khz.wav"),
		SampleRate: 8000,
	})
	rec.WriteInbound([]int16{1, 2, 3})
	if err := rec.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "8khz.wav"))
	wav, _ := ReadWAV(bytes.NewReader(raw))
	if wav.SampleRate != 8000 {
		t.Errorf("sample rate %d, want 8000", wav.SampleRate)
	}
}

// TestRecorder_GoldenWAVBytes pins down the exact byte layout of the WAV
// header + samples for a tiny known input, so a future refactor of the WAV
// writer can't silently change the wire format.
func TestRecorder_StopFailsIfDirMissing(t *testing.T) {
	t.Parallel()
	rec, _ := NewRecorder(RecorderOptions{Path: "/nonexistent-dir-bellerophon-test/x.wav"})
	rec.WriteInbound([]int16{1, 2, 3})
	if err := rec.Stop(); err == nil {
		t.Error("Stop with missing directory: want error")
	}
}

func TestRecorder_CancelOnFreshRecorderIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec, _ := NewRecorder(RecorderOptions{Path: filepath.Join(dir, "fresh.wav")})
	if err := rec.Cancel(); err != nil {
		t.Errorf("Cancel on fresh recorder: %v", err)
	}
	// Double cancel is a no-op.
	if err := rec.Cancel(); err != nil {
		t.Errorf("second Cancel: %v", err)
	}
}

func TestRecorder_GoldenWAVBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "golden.wav")
	rec, _ := NewRecorder(RecorderOptions{Path: path, SampleRate: 8000})
	rec.WriteInbound([]int16{0x1234, -1})
	rec.WriteOutbound(nil)
	if err := rec.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []byte{
		// RIFF header
		'R', 'I', 'F', 'F',
		40, 0, 0, 0, // riff size = 36 + 4 data bytes = 40
		'W', 'A', 'V', 'E',
		// fmt chunk
		'f', 'm', 't', ' ',
		16, 0, 0, 0, // chunk size
		1, 0, // PCM
		1, 0, // mono
		0x40, 0x1F, 0, 0, // 8000 Hz = 0x1F40
		0x80, 0x3E, 0, 0, // byte rate = 8000 * 1 * 2 = 16000 = 0x3E80
		2, 0, // block align = 2
		16, 0, // bits per sample
		// data chunk
		'd', 'a', 't', 'a',
		4, 0, 0, 0, // data size = 4 bytes
		0x34, 0x12, // sample 0 = 0x1234 LE
		0xFF, 0xFF, // sample 1 = -1 LE
	}
	if !bytes.Equal(got, want) {
		t.Errorf("WAV bytes mismatch:\n got %x\nwant %x", got, want)
	}
}
