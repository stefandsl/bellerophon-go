package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

// DefaultRecordingSampleRate matches what the STT pipeline consumes, so a
// recording can be replayed through Whisper for transcript audit without
// extra resampling.
const DefaultRecordingSampleRate = 16000

// RecorderOptions configures NewRecorder.
type RecorderOptions struct {
	// Path is the final destination for the WAV file. Required.
	Path string
	// SampleRate of the inbound/outbound buffers. Defaults to 16000 Hz —
	// the rate the STT pipeline operates at, so callers feeding decoded
	// G.711 should run it through codec.Resample8to16 first.
	SampleRate int
	// Logger is optional; nil is silent.
	Logger bellog.Logger
}

// Recorder accumulates the inbound (caller) and outbound (bot) audio streams
// of a call in memory and finalizes them to a single mono PCM16 WAV on Stop().
// Finalization is atomic: the file is first written to ${Path}.partial, then
// renamed to Path. On crash, the partial file may be left behind; the same
// path is re-tried on the next Stop().
type Recorder struct {
	path        string
	partialPath string
	rate        int
	logger      bellog.Logger

	mu        sync.Mutex
	inbound   []int16 // append-only
	outbound  []int16 // append-only
	finalized bool
	cancelled bool
}

// NewRecorder builds a recorder with the given options.
func NewRecorder(opts RecorderOptions) (*Recorder, error) {
	if opts.Path == "" {
		return nil, errors.New("recorder: Path is required")
	}
	rate := opts.SampleRate
	if rate <= 0 {
		rate = DefaultRecordingSampleRate
	}
	return &Recorder{
		path:        opts.Path,
		partialPath: opts.Path + ".partial",
		rate:        rate,
		logger:      opts.Logger,
	}, nil
}

// WriteInbound appends caller-side PCM16 samples. Safe to call concurrently
// with WriteOutbound. Silently no-ops after Stop or Cancel.
func (r *Recorder) WriteInbound(samples []int16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized || r.cancelled {
		return
	}
	r.inbound = append(r.inbound, samples...)
}

// WriteOutbound appends bot-side PCM16 samples. Same concurrency/no-op
// contract as WriteInbound.
func (r *Recorder) WriteOutbound(samples []int16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized || r.cancelled {
		return
	}
	r.outbound = append(r.outbound, samples...)
}

// Stop finalizes the recording: mixes inbound + outbound (sum with int16
// saturation), writes a PCM16 mono WAV to ${Path}.partial, then atomically
// renames it to Path. Idempotent — second Stop is a no-op. Returns an error
// if the recorder was Cancel'd or if the I/O fails.
func (r *Recorder) Stop() error {
	r.mu.Lock()
	if r.cancelled {
		r.mu.Unlock()
		return errors.New("recorder: already cancelled")
	}
	if r.finalized {
		r.mu.Unlock()
		return nil
	}
	r.finalized = true
	in := r.inbound
	out := r.outbound
	r.mu.Unlock()

	mixed := mixStreams(in, out)

	f, err := os.Create(r.partialPath)
	if err != nil {
		return fmt.Errorf("recorder: create partial: %w", err)
	}
	if werr := writeWAVPCM16Mono(f, mixed, r.rate); werr != nil {
		_ = f.Close()
		_ = os.Remove(r.partialPath)
		return fmt.Errorf("recorder: write WAV: %w", werr)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(r.partialPath)
		return fmt.Errorf("recorder: close partial: %w", err)
	}
	if err := os.Rename(r.partialPath, r.path); err != nil {
		// Leave .partial in place — caller can inspect.
		return fmt.Errorf("recorder: rename to final: %w", err)
	}
	if r.logger != nil {
		r.logger.Info("recording finalized",
			"path", r.path, "samples", len(mixed), "duration_ms", 1000*len(mixed)/r.rate)
	}
	return nil
}

// Cancel discards any buffered audio and removes the partial file if it
// exists. Idempotent.
func (r *Recorder) Cancel() error {
	r.mu.Lock()
	if r.cancelled {
		r.mu.Unlock()
		return nil
	}
	r.cancelled = true
	r.inbound = nil
	r.outbound = nil
	r.mu.Unlock()
	if err := os.Remove(r.partialPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("recorder: remove partial: %w", err)
	}
	return nil
}

// mixStreams produces a single PCM16 stream from two parallel sources by
// summing aligned samples and saturating at int16 bounds. The shorter input
// is zero-padded — exactly what a half-duplex stretch of the call needs.
func mixStreams(a, b []int16) []int16 {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		var sum int32
		if i < len(a) {
			sum += int32(a[i])
		}
		if i < len(b) {
			sum += int32(b[i])
		}
		switch {
		case sum > 32767:
			out[i] = 32767
		case sum < -32768:
			out[i] = -32768
		default:
			out[i] = int16(sum)
		}
	}
	return out
}

// writeWAVPCM16Mono emits a standard RIFF/WAVE header followed by PCM16
// samples. Mono, signed 16-bit little-endian — the canonical "speech" format.
func writeWAVPCM16Mono(w io.Writer, samples []int16, sampleRate int) error {
	const channels = 1
	const bitsPerSample = 16
	dataSize := len(samples) * 2
	if dataSize > int(^uint32(0)) {
		return errors.New("recorder: data too large for 32-bit RIFF size")
	}
	riffSize := uint32(36 + dataSize) //nolint:gosec // bounded above

	// RIFF header
	if _, err := w.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, riffSize); err != nil {
		return err
	}
	if _, err := w.Write([]byte("WAVE")); err != nil {
		return err
	}

	// fmt chunk
	if _, err := w.Write([]byte("fmt ")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(1)); err != nil { // PCM
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(channels)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(sampleRate)); err != nil { //nolint:gosec // bounded
		return err
	}
	byteRate := uint32(sampleRate * channels * bitsPerSample / 8) //nolint:gosec
	if err := binary.Write(w, binary.LittleEndian, byteRate); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(channels*bitsPerSample/8)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(bitsPerSample)); err != nil {
		return err
	}

	// data chunk
	if _, err := w.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(dataSize)); err != nil { //nolint:gosec // bounded
		return err
	}
	// Write samples in a single allocation rather than 1-by-1 binary.Write.
	buf := make([]byte, dataSize)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(s)) //nolint:gosec // int16 reinterpret
	}
	if _, err := w.Write(buf); err != nil {
		return err
	}
	return nil
}
