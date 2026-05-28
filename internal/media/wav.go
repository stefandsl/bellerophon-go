// Package media implements WAV file reading and the playback scheduler used
// by the Bellerophon voice stack to drive RTP from local audio assets.
package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// WAV is a decoded PCM WAV file. Samples are always int16 — 8-bit unsigned
// inputs are widened on read and centered on zero. For stereo inputs the
// samples are interleaved (L, R, L, R, …).
type WAV struct {
	SampleRate int     // Hz; common values: 8000, 16000, 22050, 44100, 48000.
	Channels   int     // 1 = mono, 2 = stereo (max for now).
	BitDepth   int     // Original bit depth (8 or 16); reflects the file, not the in-memory representation.
	Samples    []int16 // PCM16 samples, interleaved per channel.
}

// wavAudioFormatPCM is the WAV "AudioFormat" tag for plain PCM (the only
// format ReadWAV accepts).
const wavAudioFormatPCM uint16 = 1

// ReadWAVFile is the convenience wrapper around ReadWAV.
func ReadWAVFile(path string) (*WAV, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadWAV(f)
}

// ReadWAV parses a RIFF/WAVE PCM file. Non-PCM formats (µ-law, A-law,
// float, IMA-ADPCM, MP3-in-WAV, …) are rejected with a message pointing
// the operator at ffmpeg. Other audio container formats (MP3, M4A, OGG)
// fail the RIFF magic check.
func ReadWAV(r io.Reader) (*WAV, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("wav: read RIFF header: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" {
		return nil, errors.New("wav: not a RIFF/WAVE file — only PCM WAV is supported, " +
			"convert other formats with `ffmpeg -i input.<ext> -ar 16000 -ac 1 output.wav`")
	}
	if string(hdr[8:12]) != "WAVE" {
		return nil, errors.New("wav: RIFF container is not WAVE")
	}

	var (
		fmtSeen     bool
		audioFormat uint16
		channels    uint16
		sampleRate  uint32
		bitDepth    uint16
		dataBytes   []byte
	)

	for {
		var chunkHdr [8]byte
		_, err := io.ReadFull(r, chunkHdr[:])
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("wav: read chunk header: %w", err)
		}
		id := string(chunkHdr[0:4])
		size := binary.LittleEndian.Uint32(chunkHdr[4:8])

		switch id {
		case "fmt ":
			body := make([]byte, size)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, fmt.Errorf("wav: read fmt chunk: %w", err)
			}
			if size < 16 {
				return nil, fmt.Errorf("wav: fmt chunk too small (%d bytes)", size)
			}
			audioFormat = binary.LittleEndian.Uint16(body[0:2])
			channels = binary.LittleEndian.Uint16(body[2:4])
			sampleRate = binary.LittleEndian.Uint32(body[4:8])
			// byte rate (4) + block align (2) skipped — we recompute downstream.
			bitDepth = binary.LittleEndian.Uint16(body[14:16])
			fmtSeen = true
		case "data":
			body := make([]byte, size)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, fmt.Errorf("wav: read data chunk: %w", err)
			}
			dataBytes = body
		default:
			// Unknown chunk (LIST, fact, JUNK, bext, …) — skip its payload.
			if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
				return nil, fmt.Errorf("wav: skip %q chunk: %w", id, err)
			}
		}
		// WAV chunks are padded to even length.
		if size%2 == 1 {
			if _, err := io.CopyN(io.Discard, r, 1); err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("wav: skip chunk pad: %w", err)
			}
		}
	}

	if !fmtSeen {
		return nil, errors.New("wav: missing fmt chunk")
	}
	if dataBytes == nil {
		return nil, errors.New("wav: missing data chunk")
	}
	if audioFormat != wavAudioFormatPCM {
		return nil, fmt.Errorf("wav: audio format %d is not plain PCM — re-encode "+
			"with `ffmpeg -i input.wav -c:a pcm_s16le output.wav`", audioFormat)
	}
	if channels < 1 || channels > 2 {
		return nil, fmt.Errorf("wav: %d channels not supported (need 1 or 2)", channels)
	}
	if bitDepth != 8 && bitDepth != 16 {
		return nil, fmt.Errorf("wav: %d-bit PCM not supported (need 8 or 16)", bitDepth)
	}

	w := &WAV{
		SampleRate: int(sampleRate),
		Channels:   int(channels),
		BitDepth:   int(bitDepth),
	}

	switch bitDepth {
	case 16:
		if len(dataBytes)%2 != 0 {
			return nil, fmt.Errorf("wav: 16-bit data length %d is odd", len(dataBytes))
		}
		w.Samples = make([]int16, len(dataBytes)/2)
		for i := range w.Samples {
			w.Samples[i] = int16(binary.LittleEndian.Uint16(dataBytes[2*i:]))
		}
	case 8:
		// WAV 8-bit PCM is unsigned, centered at 128. Widen to signed int16.
		w.Samples = make([]int16, len(dataBytes))
		for i, b := range dataBytes {
			w.Samples[i] = int16(int(b)-128) << 8 //nolint:gosec // bounded
		}
	}
	return w, nil
}

// ToMono returns a mono copy of w. For mono inputs this is a no-op clone.
// For stereo, channels are averaged sample-by-sample.
func (w *WAV) ToMono() *WAV {
	if w.Channels == 1 {
		out := &WAV{SampleRate: w.SampleRate, Channels: 1, BitDepth: w.BitDepth}
		out.Samples = append([]int16(nil), w.Samples...)
		return out
	}
	if w.Channels != 2 {
		// ReadWAV rejects this; defensive belt-and-braces only.
		return w
	}
	pairs := len(w.Samples) / 2
	out := &WAV{
		SampleRate: w.SampleRate,
		Channels:   1,
		BitDepth:   w.BitDepth,
		Samples:    make([]int16, pairs),
	}
	for i := 0; i < pairs; i++ {
		l := int32(w.Samples[2*i])
		r := int32(w.Samples[2*i+1])
		out.Samples[i] = int16((l + r) / 2)
	}
	return out
}
