package llm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SessionStore maps a callID (e.g. a SIP call UUID) to an opaque session
// payload kept by one of the LLM backends. Anthropic stores the full
// conversation history serialized as JSON here; Bridge stores the short
// session id string returned by claude-api-server.
//
// The store has two layers:
//
//  1. An in-memory map for hot path lookups during an active call.
//  2. An optional append-only JSONL file so sessions survive a binary
//     restart — the Pi 5 hot-reloads bellerophon during deploys and a
//     mid-call restart should be able to resume the conversation rather
//     than lose its context.
//
// Each Put/Delete appends one line; Load replays them at startup with
// last-write-wins semantics. Compaction is out of scope for M002 —
// session lifetimes are bounded by call duration (a few minutes) so
// the journal stays small for the use case M002 ships.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]string
	path     string
	w        *os.File
	bw       *bufio.Writer
}

// sessionRecord is the on-disk JSONL line shape. Deleted records carry
// Deleted=true so the replay logic can drop the key without needing a
// separate tombstone format.
type sessionRecord struct {
	CallID    string `json:"call_id"`
	Data      string `json:"data,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// NewSessionStore returns an in-memory-only store (no journal). Use this
// in tests or when persistence isn't required.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]string{}}
}

// OpenSessionStore returns a store backed by the JSONL file at path.
// Missing files are created (the parent directory is mkdir'd if absent)
// so callers can point at a fresh path on first run. Existing records
// are replayed into memory before the function returns, so Get works
// immediately after the call.
//
// path == "" returns a NewSessionStore-equivalent in-memory store; this
// makes the calling code uniform whether persistence is configured or
// not.
func OpenSessionStore(path string) (*SessionStore, error) {
	if path == "" {
		return NewSessionStore(), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("llm: session store mkdir: %w", err)
	}
	// Replay first, then open for append. Doing the read-pass before
	// the writer takes over keeps the file descriptor simple — we
	// never need to seek while serving live traffic.
	sessions, err := replay(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("llm: session store open: %w", err)
	}
	return &SessionStore{
		sessions: sessions,
		path:     path,
		w:        f,
		bw:       bufio.NewWriter(f),
	}, nil
}

func replay(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("llm: session store replay open: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Allow up to 4 MiB per line — a long Anthropic history can run
	// well past the default 64 KiB. Empirically a 20-turn conversation
	// fits inside a few hundred KiB.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec sessionRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// One corrupt line shouldn't kill the whole journal —
			// skip it and let the operator notice via metrics later.
			continue
		}
		if rec.CallID == "" {
			continue
		}
		if rec.Deleted {
			delete(out, rec.CallID)
			continue
		}
		out[rec.CallID] = rec.Data
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("llm: session store scan: %w", err)
	}
	return out, nil
}

// Get returns the stored payload for callID. ok is false when no record
// (or a deleted record) is in memory.
func (s *SessionStore) Get(callID string) (data string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok = s.sessions[callID]
	return
}

// Put writes (or overwrites) the payload for callID. If a journal is
// attached the new state is appended and flushed before Put returns, so
// a crash immediately after won't lose the turn that just succeeded.
func (s *SessionStore) Put(callID, data string) error {
	if callID == "" {
		return errors.New("llm: SessionStore.Put: empty callID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[callID] = data
	return s.append(sessionRecord{
		CallID:    callID,
		Data:      data,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// Delete removes the entry for callID from memory and journals a
// tombstone. Calling Delete on an unknown callID still appends a
// tombstone — harmless, and means EndSession can be wired up without
// having to first probe Get.
func (s *SessionStore) Delete(callID string) error {
	if callID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, callID)
	return s.append(sessionRecord{
		CallID:    callID,
		Deleted:   true,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// Close flushes the buffer and closes the underlying file. Safe to call
// on a memory-only store (no-op) and safe to call more than once.
func (s *SessionStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bw == nil {
		return nil
	}
	if err := s.bw.Flush(); err != nil {
		_ = s.w.Close()
		s.w, s.bw = nil, nil
		return err
	}
	err := s.w.Close()
	s.w, s.bw = nil, nil
	return err
}

// append serializes rec and writes it through the buffered writer. The
// caller is expected to be holding s.mu. Flushed immediately so each
// successful turn is durable at the moment Put/Delete returns.
func (s *SessionStore) append(rec sessionRecord) error {
	if s.bw == nil {
		return nil
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("llm: session store marshal: %w", err)
	}
	if _, err := s.bw.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("llm: session store write: %w", err)
	}
	return s.bw.Flush()
}
