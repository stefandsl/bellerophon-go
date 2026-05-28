package conversation

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNewTranscriptWriter_RequiresFields(t *testing.T) {
	t.Parallel()
	if _, err := NewTranscriptWriter("", "call"); err == nil {
		t.Error("empty dir: expected error")
	}
	if _, err := NewTranscriptWriter(t.TempDir(), ""); err == nil {
		t.Error("empty callID: expected error")
	}
}

func TestNewTranscriptWriter_CreatesNestedDir(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "a", "b", "c")
	w, err := NewTranscriptWriter(root, "call-1")
	if err != nil {
		t.Fatalf("NewTranscriptWriter: %v", err)
	}
	defer w.Close()
	if _, err := os.Stat(root); err != nil {
		t.Errorf("nested dir not created: %v", err)
	}
	if !strings.HasSuffix(w.Path(), "call-1.jsonl") {
		t.Errorf("Path = %q, want .../call-1.jsonl", w.Path())
	}
}

func TestTranscriptWriter_AppendAndReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := NewTranscriptWriter(dir, "uuid")
	if err != nil {
		t.Fatalf("NewTranscriptWriter: %v", err)
	}
	turns := []Turn{
		{Turn: 1, Role: "user", Text: "hello", TimestampMs: 100},
		{Turn: 1, Role: "assistant", Text: "hi", TimestampMs: 200},
		{Turn: 2, Role: "user", Text: "goodbye", TimestampMs: 300},
	}
	for _, tn := range turns {
		if err := w.Append(tn); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read back & verify shape.
	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var got []Turn
	for sc.Scan() {
		var tn Turn
		if err := json.Unmarshal(sc.Bytes(), &tn); err != nil {
			t.Fatalf("unmarshal %q: %v", sc.Text(), err)
		}
		got = append(got, tn)
	}
	if len(got) != len(turns) {
		t.Fatalf("len = %d, want %d", len(got), len(turns))
	}
	for i, want := range turns {
		if got[i] != want {
			t.Errorf("turn[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestTranscriptWriter_AppendsAcrossReopens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w1, err := NewTranscriptWriter(dir, "uuid")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = w1.Append(Turn{Turn: 1, Role: "user", Text: "first"})
	_ = w1.Close()

	w2, _ := NewTranscriptWriter(dir, "uuid")
	defer w2.Close()
	_ = w2.Append(Turn{Turn: 2, Role: "user", Text: "second"})
	_ = w2.Close()

	data, err := os.ReadFile(filepath.Join(dir, "uuid.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Errorf("lines = %d, want 2 (append mode must preserve previous turns)", len(lines))
	}
}

func TestTranscriptWriter_NilReceiverIsNoop(t *testing.T) {
	t.Parallel()
	var w *TranscriptWriter
	if err := w.Append(Turn{}); err != nil {
		t.Errorf("nil Append: %v, want nil", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("nil Close: %v, want nil", err)
	}
}

func TestTranscriptWriter_AfterCloseIsRejected(t *testing.T) {
	t.Parallel()
	w, _ := NewTranscriptWriter(t.TempDir(), "c")
	_ = w.Close()
	if err := w.Append(Turn{Text: "x"}); err == nil {
		t.Error("Append after Close: expected error")
	}
}

func TestTranscriptWriter_ConcurrentAppends(t *testing.T) {
	t.Parallel()
	w, _ := NewTranscriptWriter(t.TempDir(), "c")
	defer w.Close()
	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_ = w.Append(Turn{Turn: i, Role: "user", Text: "x"})
		}()
	}
	wg.Wait()

	// Verify the file has exactly N lines and each line is parseable.
	data, _ := os.ReadFile(w.Path())
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != N {
		t.Errorf("lines = %d, want %d", len(lines), N)
	}
	for _, ln := range lines {
		var tn Turn
		if err := json.Unmarshal([]byte(ln), &tn); err != nil {
			t.Errorf("corrupt line under concurrency: %q (%v)", ln, err)
		}
	}
}

func TestSanitizeCallID_RejectsPathSeparators(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"normal-id":     "normal-id",
		"../etc/passwd": ".._etc_passwd",
		"a\\b":          "a_b",
		"":              "_invalid_",
		".":             "_invalid_",
		"..":            "_invalid_",
		"with\x00null":  "with_null",
	} {
		if got := sanitizeCallID(in); got != want {
			t.Errorf("sanitizeCallID(%q) = %q, want %q", in, got, want)
		}
	}
}
