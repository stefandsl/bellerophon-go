package media

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// buildWAV synthesizes a minimal valid PCM WAV file: RIFF + fmt + data.
func buildWAV(t *testing.T, channels uint16, sampleRate uint32, bitDepth uint16, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer

	// data chunk size = len(data). fmt chunk size = 16.
	// total RIFF size = 4 ("WAVE") + 8+16 (fmt chunk header + body) + 8+len(data) = 36 + len(data)
	riffSize := uint32(36 + len(data)) //nolint:gosec // bounded by test data
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, riffSize)
	buf.WriteString("WAVE")

	// fmt chunk
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16)) // fmt chunk size
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // PCM
	_ = binary.Write(&buf, binary.LittleEndian, channels)
	_ = binary.Write(&buf, binary.LittleEndian, sampleRate)
	byteRate := sampleRate * uint32(channels) * uint32(bitDepth/8)
	_ = binary.Write(&buf, binary.LittleEndian, byteRate)
	blockAlign := channels * (bitDepth / 8)
	_ = binary.Write(&buf, binary.LittleEndian, blockAlign)
	_ = binary.Write(&buf, binary.LittleEndian, bitDepth)

	// data chunk
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(data))) //nolint:gosec // bounded
	buf.Write(data)
	return buf.Bytes()
}

func TestReadWAV_Mono16(t *testing.T) {
	t.Parallel()
	// 4 samples at 8 kHz mono 16-bit, values {0, 100, -100, 32767}
	samples := []int16{0, 100, -100, 32767}
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s)) //nolint:gosec // int16 reinterpret
	}
	raw := buildWAV(t, 1, 8000, 16, data)

	got, err := ReadWAV(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if got.SampleRate != 8000 || got.Channels != 1 || got.BitDepth != 16 {
		t.Fatalf("metadata: rate=%d ch=%d bd=%d", got.SampleRate, got.Channels, got.BitDepth)
	}
	if len(got.Samples) != len(samples) {
		t.Fatalf("samples len=%d, want %d", len(got.Samples), len(samples))
	}
	for i, s := range samples {
		if got.Samples[i] != s {
			t.Errorf("sample[%d] = %d, want %d", i, got.Samples[i], s)
		}
	}
}

func TestReadWAV_Mono8Unsigned(t *testing.T) {
	t.Parallel()
	// 8-bit unsigned PCM: 128 = 0, 0 = -32768-ish, 255 = +32512-ish
	data := []byte{128, 129, 127, 255, 0}
	raw := buildWAV(t, 1, 8000, 8, data)
	got, err := ReadWAV(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if got.BitDepth != 8 || len(got.Samples) != 5 {
		t.Fatalf("got bd=%d len=%d", got.BitDepth, len(got.Samples))
	}
	if got.Samples[0] != 0 {
		t.Errorf("128 unsigned should map to 0; got %d", got.Samples[0])
	}
	if got.Samples[1] != 256 || got.Samples[2] != -256 {
		t.Errorf("129/127 → got %d/%d, want 256/-256", got.Samples[1], got.Samples[2])
	}
	if got.Samples[3] != (255-128)<<8 || got.Samples[4] != (0-128)<<8 {
		t.Errorf("255/0 widen: got %d/%d", got.Samples[3], got.Samples[4])
	}
}

func TestReadWAV_Stereo16ToMono(t *testing.T) {
	t.Parallel()
	// Two stereo frames: (1000, 2000) and (-1000, -3000). Mono mix → (1500, -2000).
	frames := []int16{1000, 2000, -1000, -3000}
	data := make([]byte, len(frames)*2)
	for i, s := range frames {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s)) //nolint:gosec // int16 reinterpret
	}
	raw := buildWAV(t, 2, 16000, 16, data)
	got, err := ReadWAV(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if got.Channels != 2 {
		t.Fatalf("channels=%d, want 2", got.Channels)
	}
	mono := got.ToMono()
	if mono.Channels != 1 {
		t.Fatalf("ToMono channels=%d", mono.Channels)
	}
	wantMono := []int16{1500, -2000}
	if len(mono.Samples) != len(wantMono) {
		t.Fatalf("ToMono len=%d, want %d", len(mono.Samples), len(wantMono))
	}
	for i, s := range wantMono {
		if mono.Samples[i] != s {
			t.Errorf("mono[%d] = %d, want %d", i, mono.Samples[i], s)
		}
	}
}

func TestReadWAV_RejectsNonRIFF(t *testing.T) {
	t.Parallel()
	// MP3 starts with ID3 or 0xFF 0xFB; OGG with "OggS"; M4A with ftyp box.
	for _, junk := range [][]byte{
		[]byte("ID3\x04\x00\x00\x00\x00\x00\x00\x00\x00"), // MP3 ID3
		[]byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00"),    // OGG
		bytes.Repeat([]byte{0x00}, 12),                    // pure zeros
	} {
		_, err := ReadWAV(bytes.NewReader(junk))
		if err == nil {
			t.Errorf("ReadWAV accepted non-RIFF input: %q", junk[:4])
		}
		if !strings.Contains(err.Error(), "ffmpeg") {
			t.Errorf("error message should mention ffmpeg; got %q", err.Error())
		}
	}
}

func TestReadWAV_RejectsNonPCMFormat(t *testing.T) {
	t.Parallel()
	// Build a WAV but flip audio format to 3 (IEEE float).
	raw := buildWAV(t, 1, 8000, 16, []byte{0, 0, 0, 0})
	// audio format is at offset 20 (RIFF 12 + "fmt " 4 + size 4 = 20)
	raw[20] = 3
	_, err := ReadWAV(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("ReadWAV accepted non-PCM format")
	}
	if !strings.Contains(err.Error(), "not plain PCM") {
		t.Errorf("error should mention non-PCM; got %q", err.Error())
	}
}

func TestReadWAV_RejectsTooManyChannels(t *testing.T) {
	t.Parallel()
	raw := buildWAV(t, 6, 8000, 16, []byte{0, 0})
	_, err := ReadWAV(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("ReadWAV accepted 6-channel")
	}
}

func TestReadWAVFile_RoundTripFromDisk(t *testing.T) {
	t.Parallel()
	samples := []int16{0, 100, -100, 32767, -32768}
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s)) //nolint:gosec // int16 reinterpret
	}
	raw := buildWAV(t, 1, 8000, 16, data)

	dir := t.TempDir()
	path := dir + "/test.wav"
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write temp wav: %v", err)
	}
	got, err := ReadWAVFile(path)
	if err != nil {
		t.Fatalf("ReadWAVFile: %v", err)
	}
	if len(got.Samples) != len(samples) {
		t.Fatalf("samples len=%d, want %d", len(got.Samples), len(samples))
	}

	// ReadWAVFile on a missing path must surface the OS error.
	if _, err := ReadWAVFile(dir + "/nope.wav"); err == nil {
		t.Error("ReadWAVFile on missing file: expected error")
	}
}

func TestReadWAV_SkipsUnknownChunks(t *testing.T) {
	t.Parallel()
	// Build a base WAV, then insert a LIST chunk between fmt and data.
	samples := []int16{42, -42}
	data := make([]byte, 4)
	binary.LittleEndian.PutUint16(data[0:], uint16(samples[0])) //nolint:gosec
	binary.LittleEndian.PutUint16(data[2:], uint16(samples[1])) //nolint:gosec

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	// We don't bother getting riff-size exactly right; readers ignore it.
	_ = binary.Write(&buf, binary.LittleEndian, uint32(1000))
	buf.WriteString("WAVE")

	// fmt
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // 1 ch
	_ = binary.Write(&buf, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))

	// LIST chunk to skip
	buf.WriteString("LIST")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(8))
	buf.WriteString("INFOIART") // arbitrary payload

	// data
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)

	got, err := ReadWAV(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadWAV with unknown chunk: %v", err)
	}
	if len(got.Samples) != 2 || got.Samples[0] != 42 || got.Samples[1] != -42 {
		t.Errorf("samples wrong: %v", got.Samples)
	}
}
