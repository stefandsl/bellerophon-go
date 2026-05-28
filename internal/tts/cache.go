package tts

import (
	"context"
	"crypto/md5" //nolint:gosec // md5 is a content key, not a security primitive
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultCacheCap matches voice-app/lib/tts-service.js's in-process
// cap. With ~100 KiB per second of PCM16/16k, 1000 entries is a hard
// ceiling around ~5 GiB if every entry were a 50 s monologue —
// realistic voice replies are 3-10 s, so the actual footprint stays
// comfortably under 1 GiB before LRU eviction starts.
const DefaultCacheCap = 1000

// Cache is the dual in-memory + on-disk cache for synthesized audio.
// Keys are md5(text|voiceID|modelID) hex strings (same scheme
// voice-app uses, so the two stacks would key-compatibly share a
// cache directory if ever co-located). Values are raw PCM16 mono
// bytes at SampleRate Hz.
//
// The cache adds two behaviours on top of plain file storage:
//
//  1. Single-flight: concurrent Generate calls for the same key
//     coalesce into one upstream synthesis. This matters during
//     batch campaigns that fire N workers on a static opener — pay
//     for one synthesis, not N.
//  2. LRU bound on the in-memory index: file mtimes are touched on
//     each hit so an external janitor (later milestone) can evict by
//     age without invalidating live entries.
//
// Disk format is `<dir>/<key>.pcm` — raw bytes, no header. The cost
// of skipping a header is one less spec for the playback path to
// know about; SampleRate is implied by the package.
type Cache struct {
	dir      string
	cap      int
	mu       sync.Mutex
	entries  map[string]*cacheEntry
	order    []string // hottest at tail; trimmed from head on overflow
	inflight map[string]*inflightSlot
}

type cacheEntry struct {
	path    string
	mtimeMs int64
}

type inflightSlot struct {
	done chan struct{}
	data []byte
	err  error
}

// NewCache builds a cache rooted at dir. The directory is created if
// missing — first-call mkdir keeps the calling code simple. capacity
// <= 0 selects DefaultCacheCap.
func NewCache(dir string, capacity int) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("tts: cache dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tts: cache mkdir %q: %w", dir, err)
	}
	if capacity <= 0 {
		capacity = DefaultCacheCap
	}
	return &Cache{
		dir:      dir,
		cap:      capacity,
		entries:  map[string]*cacheEntry{},
		inflight: map[string]*inflightSlot{},
	}, nil
}

// Key derives the cache key from the synthesis inputs. Exported so
// callers can probe the cache without going through Generate (used by
// the prewarm path: "does this opener already exist?").
func Key(text, voiceID, modelID string) string {
	h := md5.Sum([]byte(text + "|" + voiceID + "|" + modelID)) //nolint:gosec
	return hex.EncodeToString(h[:])
}

// Generate returns the PCM bytes for key, calling render on a cache
// miss. render is the actual upstream synthesis closure; the Cache
// stays oblivious to which provider produced the audio.
//
// Concurrent calls for the same key block on the first render and
// share its result. If the first render fails, every waiter receives
// the same error and the slot is released so the next call retries.
func (c *Cache) Generate(ctx context.Context, key string, render func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if key == "" {
		return nil, errors.New("tts: empty cache key")
	}

	// 1. Fast path: in-memory entry + file still on disk.
	if pcm, ok := c.tryHit(key); ok {
		return pcm, nil
	}

	// 2. Single-flight: do we already have a render in flight?
	c.mu.Lock()
	slot, exists := c.inflight[key]
	if !exists {
		slot = &inflightSlot{done: make(chan struct{})}
		c.inflight[key] = slot
	}
	c.mu.Unlock()

	if exists {
		// Wait for the leader to finish.
		select {
		case <-slot.done:
			return slot.data, slot.err
		case <-ctx.Done():
			// Caller giving up; the leader still completes for the
			// next waiter, so we don't drop the in-flight slot here.
			return nil, ctx.Err()
		}
	}

	// We're the leader. Render, persist, and broadcast.
	pcm, err := render(ctx)
	if err == nil {
		if perr := c.persist(key, pcm); perr != nil {
			// A persistence failure doesn't fail the call — the
			// caller still got valid audio. Surfacing the error
			// would make a full disk break TTS entirely, which is
			// strictly worse than running with cache misses.
			_ = perr
		}
	}

	c.mu.Lock()
	slot.data = pcm
	slot.err = err
	close(slot.done)
	delete(c.inflight, key)
	c.mu.Unlock()

	return pcm, err
}

// Put writes pcm to the cache directly (no synthesis). Useful for
// tests and for warming the cache from a recorded canned phrase.
func (c *Cache) Put(key string, pcm []byte) error {
	if key == "" {
		return errors.New("tts: empty cache key")
	}
	return c.persist(key, pcm)
}

// Has reports whether key is currently cached on disk. The check
// returns true only if the file is readable — a stale in-memory
// entry pointing at a missing file is dropped.
func (c *Cache) Has(key string) bool {
	_, ok := c.tryHit(key)
	return ok
}

// Path returns the on-disk path for key. The file may or may not
// exist; callers using this are expected to handle ENOENT. Keys
// containing path separators or relative-path components are
// flattened to the bare basename — Key() returns hex md5 so this
// never matters in production, but it makes the function safe to
// call on arbitrary external input.
func (c *Cache) Path(key string) string {
	// Strip every "/" or "\" and reject "." / ".." outright so a
	// crafted key like "../../etc/passwd" can't escape c.dir.
	safe := strings.NewReplacer("/", "_", "\\", "_", "\x00", "_").Replace(key)
	if safe == "" || safe == "." || safe == ".." {
		safe = "_invalid_"
	}
	return filepath.Join(c.dir, safe+".pcm")
}

func (c *Cache) tryHit(key string) ([]byte, bool) {
	c.mu.Lock()
	entry, ok := c.entries[key]
	c.mu.Unlock()

	path := c.Path(key)
	if !ok {
		// In-memory miss; the file might still exist from a previous
		// process. Try to adopt it.
		if _, err := os.Stat(path); err != nil {
			return nil, false
		}
		// Found on disk — fall through to read + index.
	}

	data, err := os.ReadFile(path) //nolint:gosec // path is dir+key, no traversal
	if err != nil {
		// Stale in-memory entry; evict.
		c.mu.Lock()
		delete(c.entries, key)
		c.removeFromOrder(key)
		c.mu.Unlock()
		return nil, false
	}

	// Touch mtime so external janitors keep this entry alive.
	now := time.Now()
	_ = os.Chtimes(path, now, now)

	c.mu.Lock()
	if entry == nil {
		entry = &cacheEntry{path: path}
		c.entries[key] = entry
	}
	entry.mtimeMs = now.UnixMilli()
	c.bumpOrder(key)
	c.mu.Unlock()
	return data, true
}

func (c *Cache) persist(key string, pcm []byte) error {
	path := c.Path(key)
	// Write to a temp file then rename so a crash mid-write can't
	// leave a half-cached entry that subsequent hits treat as valid.
	tmp, err := os.CreateTemp(c.dir, ".tts-*.tmp")
	if err != nil {
		return fmt.Errorf("tts: cache tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(pcm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("tts: cache write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("tts: cache close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("tts: cache rename: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &cacheEntry{path: path, mtimeMs: time.Now().UnixMilli()}
	c.bumpOrder(key)
	c.evictOverflow()
	return nil
}

// bumpOrder moves key to the tail of the LRU list. Caller holds mu.
func (c *Cache) bumpOrder(key string) {
	c.removeFromOrder(key)
	c.order = append(c.order, key)
}

func (c *Cache) removeFromOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// evictOverflow trims the LRU list back to capacity. Disk files are
// left alone — only the in-memory index is bounded. The on-disk
// directory is the source of truth for "what we paid to render"; an
// operator-side cleanup process is the right place to age files out.
func (c *Cache) evictOverflow() {
	for len(c.order) > c.cap {
		victim := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, victim)
	}
}
