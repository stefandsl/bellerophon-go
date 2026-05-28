package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionStore_MemoryOnly(t *testing.T) {
	t.Parallel()
	s := NewSessionStore()
	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get on unknown key returned ok=true")
	}
	if err := s.Put("call-1", "history-blob"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := s.Get("call-1")
	if !ok || got != "history-blob" {
		t.Errorf("Get = (%q, %v), want (history-blob, true)", got, ok)
	}
	if err := s.Delete("call-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get("call-1"); ok {
		t.Error("Get after Delete still returned ok=true")
	}
}

func TestSessionStore_PutRejectsEmptyCallID(t *testing.T) {
	t.Parallel()
	s := NewSessionStore()
	if err := s.Put("", "x"); err == nil {
		t.Error("Put with empty callID: expected error")
	}
}

func TestSessionStore_DeleteEmptyCallIDIsNoop(t *testing.T) {
	t.Parallel()
	s := NewSessionStore()
	if err := s.Delete(""); err != nil {
		t.Errorf("Delete(\"\"): expected nil error, got %v", err)
	}
}

func TestOpenSessionStore_EmptyPathReturnsMemory(t *testing.T) {
	t.Parallel()
	s, err := OpenSessionStore("")
	if err != nil {
		t.Fatalf("OpenSessionStore(\"\"): %v", err)
	}
	if s.path != "" || s.w != nil {
		t.Error("empty path should yield in-memory-only store")
	}
}

// TestOpenSessionStore_PersistsAndReplays writes, closes, re-opens and
// verifies the journal replays the latest value per callID.
func TestOpenSessionStore_PersistsAndReplays(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "sessions.jsonl")

	s, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("OpenSessionStore: %v", err)
	}
	if err := s.Put("call-A", "v1"); err != nil {
		t.Fatalf("Put A v1: %v", err)
	}
	if err := s.Put("call-B", "B-state"); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	// Overwrite A to verify last-write-wins on replay.
	if err := s.Put("call-A", "v2"); err != nil {
		t.Fatalf("Put A v2: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Calling Close twice should be safe.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	s2, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if got, _ := s2.Get("call-A"); got != "v2" {
		t.Errorf("call-A after replay = %q, want v2", got)
	}
	if got, _ := s2.Get("call-B"); got != "B-state" {
		t.Errorf("call-B after replay = %q, want B-state", got)
	}
}

// TestSessionStore_DeleteTombstoneSurvivesReplay confirms deleted keys
// stay deleted after a restart — important so EndSession at hangup
// can't be silently undone by a journal replay.
func TestSessionStore_DeleteTombstoneSurvivesReplay(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.jsonl")

	s, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Put("call-X", "live")
	_ = s.Delete("call-X")
	_ = s.Close()

	s2, _ := OpenSessionStore(path)
	t.Cleanup(func() { _ = s2.Close() })
	if _, ok := s2.Get("call-X"); ok {
		t.Error("Get after Delete+restart still returned ok=true")
	}
}

// TestReplay_SkipsCorruptLines verifies a single bad line doesn't kill
// the whole journal — operators should be able to recover from a
// partial write at the tail.
func TestReplay_SkipsCorruptLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	content := `{"call_id":"a","data":"good","updated_at":"2026-01-01T00:00:00Z"}
{not json
{"call_id":"b","data":"also good","updated_at":"2026-01-01T00:00:01Z"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got, ok := s.Get("a"); !ok || got != "good" {
		t.Errorf("a = (%q, %v); want (good, true)", got, ok)
	}
	if got, ok := s.Get("b"); !ok || got != "also good" {
		t.Errorf("b = (%q, %v); want (also good, true)", got, ok)
	}
}

// TestReplay_HandlesLargeHistory checks the 4 MiB scanner buffer applied
// in replay() — a long Anthropic conversation can exceed bufio's default
// 64 KiB ceiling.
func TestReplay_HandlesLargeHistory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.jsonl")

	s, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	big := strings.Repeat("x", 200*1024) // 200 KiB payload
	if err := s.Put("big", big); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	_ = s.Close()

	s2, err := OpenSessionStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	got, ok := s2.Get("big")
	if !ok || len(got) != len(big) {
		t.Errorf("big payload after replay: ok=%v len=%d want len=%d", ok, len(got), len(big))
	}
}
