package conversation

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Turn is one entry in a per-call transcript JSONL file. Field names
// match the M002 spec: {turn, role, text, timestamp_ms}. M003's admin
// REST endpoints consume the same shape, so changing this would
// require coordinated changes there too — keep it stable.
type Turn struct {
	Turn        int    `json:"turn"`
	Role        string `json:"role"`
	Text        string `json:"text"`
	TimestampMs int64  `json:"timestamp_ms"`
}

// TranscriptWriter appends Turn records to a JSONL file named after
// the call id, one file per call. It is safe for concurrent Append
// calls — the conversation loop is single-threaded today but the
// admin REST tail-cursor (M003) will need read-while-write safety.
//
// The file is opened in append mode and the buffer is flushed after
// every Append so a crash mid-call doesn't lose the last turn.
// Voice agents are short-lived (a few minutes per call); per-write
// flushes have negligible cost at this turn rate.
type TranscriptWriter struct {
	mu   sync.Mutex
	f    *os.File
	bw   *bufio.Writer
	path string
}

// NewTranscriptWriter opens (or creates) <dir>/<callID>.jsonl in
// append mode. The dir is mkdir'd if missing. callID is required
// and must not contain path separators — Sanitize is applied
// defensively so a malicious id can't escape dir.
func NewTranscriptWriter(dir, callID string) (*TranscriptWriter, error) {
	if dir == "" {
		return nil, errors.New("conversation: transcript dir is required")
	}
	if callID == "" {
		return nil, errors.New("conversation: callID is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("conversation: transcript mkdir: %w", err)
	}
	path := filepath.Join(dir, sanitizeCallID(callID)+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("conversation: transcript open: %w", err)
	}
	return &TranscriptWriter{
		f:    f,
		bw:   bufio.NewWriter(f),
		path: path,
	}, nil
}

// Path returns the absolute on-disk path the writer is appending to.
// Exposed for the loop's startup log and for tests that need to read
// the file back.
func (w *TranscriptWriter) Path() string { return w.path }

// Append serializes turn as one JSON line and flushes immediately.
// A nil receiver is a no-op so callers can pre-wire a writer that
// hasn't been configured yet without nil-checking at each call site.
func (w *TranscriptWriter) Append(turn Turn) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bw == nil {
		return errors.New("conversation: transcript already closed")
	}
	line, err := json.Marshal(turn)
	if err != nil {
		return fmt.Errorf("conversation: transcript marshal: %w", err)
	}
	if _, err := w.bw.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("conversation: transcript write: %w", err)
	}
	return w.bw.Flush()
}

// Close flushes any buffered bytes and closes the underlying file.
// Safe to call more than once.
func (w *TranscriptWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bw == nil {
		return nil
	}
	flushErr := w.bw.Flush()
	closeErr := w.f.Close()
	w.bw, w.f = nil, nil
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// sanitizeCallID strips path separators and reserved names so a
// crafted CallID can't escape the transcript dir.
func sanitizeCallID(s string) string {
	r := []byte(s)
	for i, b := range r {
		switch b {
		case '/', '\\', 0:
			r[i] = '_'
		}
	}
	out := string(r)
	if out == "" || out == "." || out == ".." {
		return "_invalid_"
	}
	return out
}
