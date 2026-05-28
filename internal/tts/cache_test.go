package tts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNewCache_RejectsEmptyDir(t *testing.T) {
	t.Parallel()
	if _, err := NewCache("", 0); err == nil {
		t.Error("empty dir: expected error")
	}
}

func TestNewCache_CreatesMissingDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nested", "tts-cache")
	c, err := NewCache(dir, 0)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	if _, err := os.Stat(c.dir); err != nil {
		t.Errorf("cache dir not created: %v", err)
	}
}

func TestNewCache_AppliesDefaultCap(t *testing.T) {
	t.Parallel()
	c, _ := NewCache(t.TempDir(), 0)
	if c.cap != DefaultCacheCap {
		t.Errorf("cap = %d, want %d", c.cap, DefaultCacheCap)
	}
	c2, _ := NewCache(t.TempDir(), 5)
	if c2.cap != 5 {
		t.Errorf("explicit cap not honoured: %d", c2.cap)
	}
}

func TestKey_StableAcrossInputs(t *testing.T) {
	t.Parallel()
	a := Key("hello", "voice", "model")
	b := Key("hello", "voice", "model")
	if a != b {
		t.Error("Key is not deterministic")
	}
	if Key("hello", "voice", "model") == Key("hello", "voice", "model2") {
		t.Error("Key collides across distinct models")
	}
	if len(a) != 32 {
		t.Errorf("Key length = %d, want 32 (hex md5)", len(a))
	}
}

func TestCache_GenerateMissThenHit(t *testing.T) {
	t.Parallel()
	c, _ := NewCache(t.TempDir(), 0)
	var calls atomic.Int32
	render := func(_ context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte("pcm-bytes"), nil
	}
	a, err := c.Generate(context.Background(), "k1", render)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := c.Generate(context.Background(), "k1", render)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(a) != "pcm-bytes" || string(b) != "pcm-bytes" {
		t.Errorf("data mismatch: a=%q b=%q", a, b)
	}
	if calls.Load() != 1 {
		t.Errorf("render calls = %d, want 1", calls.Load())
	}
}

func TestCache_GenerateRejectsEmptyKey(t *testing.T) {
	t.Parallel()
	c, _ := NewCache(t.TempDir(), 0)
	_, err := c.Generate(context.Background(), "", func(_ context.Context) ([]byte, error) {
		return []byte("x"), nil
	})
	if err == nil {
		t.Error("empty key: expected error")
	}
}

func TestCache_GenerateSurfacesRenderError(t *testing.T) {
	t.Parallel()
	c, _ := NewCache(t.TempDir(), 0)
	wantErr := errors.New("boom")
	_, err := c.Generate(context.Background(), "k", func(_ context.Context) ([]byte, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	// No file should be written on error.
	if _, statErr := os.Stat(c.Path("k")); !os.IsNotExist(statErr) {
		t.Errorf("expected no file after render error, stat err = %v", statErr)
	}
}

// TestCache_SingleFlight: N concurrent Generates with the same key
// invoke render exactly once.
func TestCache_SingleFlight(t *testing.T) {
	t.Parallel()
	c, _ := NewCache(t.TempDir(), 0)
	var (
		calls   atomic.Int32
		release = make(chan struct{})
	)
	render := func(_ context.Context) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte("shared"), nil
	}
	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.Generate(context.Background(), "shared-key", render)
		}()
	}
	// Yield so all goroutines arrive at the inflight slot.
	for calls.Load() == 0 {
		// busy-wait OK in test
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("render calls = %d, want 1", calls.Load())
	}
}

// TestCache_Persistence: write key in one Cache, open a fresh Cache on
// the same dir, expect the entry to be discoverable.
func TestCache_Persistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c1, _ := NewCache(dir, 0)
	if err := c1.Put("k", []byte("persisted")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	c2, _ := NewCache(dir, 0)
	if !c2.Has("k") {
		t.Error("fresh cache instance can't see persisted entry")
	}
	got, err := c2.Generate(context.Background(), "k", func(_ context.Context) ([]byte, error) {
		t.Error("render should not be called on persisted hit")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(got) != "persisted" {
		t.Errorf("data = %q", got)
	}
}

// TestCache_AtomicWrite: when render returns an error, no half-written
// file should remain in the cache dir.
func TestCache_AtomicWrite_NoHalfFileOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, _ := NewCache(dir, 0)
	_, _ = c.Generate(context.Background(), "k", func(_ context.Context) ([]byte, error) {
		return nil, errors.New("fail")
	})
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".pcm" {
			t.Errorf("unexpected .pcm file after failed render: %s", f.Name())
		}
	}
}

// TestCache_EvictsBeyondCap: in-memory index is bounded at cap. The
// disk files themselves stay — the on-disk cache is the source of
// truth for "what we paid to render".
func TestCache_EvictsBeyondCap(t *testing.T) {
	t.Parallel()
	c, _ := NewCache(t.TempDir(), 2)
	_ = c.Put("a", []byte("A"))
	_ = c.Put("b", []byte("B"))
	_ = c.Put("c", []byte("C"))
	c.mu.Lock()
	if len(c.entries) != 2 {
		t.Errorf("in-memory entries = %d, want 2", len(c.entries))
	}
	if len(c.order) != 2 {
		t.Errorf("order list = %d, want 2", len(c.order))
	}
	c.mu.Unlock()

	// All three files should still be on disk.
	for _, k := range []string{"a", "b", "c"} {
		if _, err := os.Stat(c.Path(k)); err != nil {
			t.Errorf("file %s gone: %v", k, err)
		}
	}
}

// TestCache_CtxCancelOnFollower: a follower waiting on an in-flight
// slot can cancel without blocking forever. The leader still completes.
func TestCache_CtxCancelOnFollower(t *testing.T) {
	t.Parallel()
	c, _ := NewCache(t.TempDir(), 0)
	releaseLeader := make(chan struct{})
	leaderDone := make(chan struct{})

	go func() {
		_, _ = c.Generate(context.Background(), "k", func(_ context.Context) ([]byte, error) {
			<-releaseLeader
			return []byte("ok"), nil
		})
		close(leaderDone)
	}()

	// Brief sleep so the leader registers its inflight slot first.
	for !inflightHas(c, "k") {
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Generate(ctx, "k", func(_ context.Context) ([]byte, error) {
		t.Error("follower's render should not run")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("follower err = %v, want context.Canceled", err)
	}
	close(releaseLeader)
	<-leaderDone
}

func inflightHas(c *Cache, k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.inflight[k]
	return ok
}
