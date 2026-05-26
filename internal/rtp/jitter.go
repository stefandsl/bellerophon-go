// Jitter buffer for inbound RTP. Reorders packets by RTP sequence number
// (handling 16-bit wraparound) and releases them on a steady playout clock
// so the codec consumer sees a smooth stream even when the network jitters.
//
// Design (S04 T03):
//
//   - Each packet is keyed by an extended 32-bit sequence number = roc<<16 |
//     seq, where roc (rollover counter) increments every time the 16-bit
//     sequence wraps. This mirrors SRTP §3.3.1 and keeps ordering correct
//     across the wrap.
//   - The first accepted packet anchors the buffer: its arrival time + the
//     configured TargetDelay becomes the scheduled playout for that
//     sequence. Subsequent packets play out at anchor + (extSeq-baseSeq)*
//     ptime, so loss does not warp the clock and silence-suppression
//     timestamp jumps (CNG) are absorbed.
//   - Push rejects packets whose extended sequence is <= the last popped
//     (already played or unrecoverably late) and bumps a "too late" counter.
//   - Pop releases the head only when its scheduled time has arrived. If the
//     head is already more than MaxLate past due, drop and advance so the
//     consumer never stalls behind a single straggler.
//
// Wiring into the InviteHandler lands in S04 T05.
package rtp

import (
	"container/heap"
	"sync"
	"time"
)

// Jitter-buffer defaults per M001-SPEC.md §S04.
const (
	DefaultTargetDelay = 60 * time.Millisecond
	DefaultMaxLate     = 100 * time.Millisecond
	// DefaultPtime is the inter-packet spacing used to schedule playout when
	// the codec advertises 20 ms frames (G.711 PCMU/PCMA). Override per call
	// via JBOptions.Ptime if a different codec lands later.
	DefaultPtime = 20 * time.Millisecond
)

// JBOptions configures a JitterBuffer.
type JBOptions struct {
	// TargetDelay is the playout offset applied to the first packet's
	// arrival. Zero -> DefaultTargetDelay.
	TargetDelay time.Duration
	// MaxLate is the grace window past a packet's scheduled playout before
	// it is dropped. Zero -> DefaultMaxLate.
	MaxLate time.Duration
	// Ptime is the inter-packet spacing used for scheduling. Zero ->
	// DefaultPtime.
	Ptime time.Duration
	// Capacity caps the heap size; once exceeded the oldest scheduled
	// packet is dropped to make room. Zero -> 64 (>1 s at 20 ms ptime).
	Capacity int
	// Now is the clock source. Zero -> time.Now. Injectable for tests.
	Now func() time.Time
}

// JBStats is a snapshot of buffer counters.
type JBStats struct {
	// Pushed is the count of Push calls that accepted a packet.
	Pushed uint64
	// Popped is the count of Pop calls that returned a packet.
	Popped uint64
	// DroppedLate counts packets discarded on Push for arriving below the
	// playout watermark (extSeq <= last popped).
	DroppedLate uint64
	// DroppedOverflow counts packets evicted when Capacity was exceeded.
	DroppedOverflow uint64
	// DroppedExpired counts packets discarded inside Pop because their
	// scheduled playout was more than MaxLate past due.
	DroppedExpired uint64
	// Depth is the current number of buffered packets.
	Depth int
}

// JitterBuffer reorders RTP packets and releases them on a steady clock.
// All methods are safe for concurrent use; the buffer is single-producer /
// single-consumer in practice but the locking keeps Stats lock-free for
// observability surfaces.
type JitterBuffer struct {
	target  time.Duration
	maxLate time.Duration
	ptime   time.Duration
	cap     int
	now     func() time.Time

	mu sync.Mutex
	h  jbHeap

	// Sequence-extension state. set is false until the first packet.
	set         bool
	roc         uint32 // rollover counter
	lastSeen    uint16 // last raw sequence pushed (for wrap detection)
	baseExtSeq  uint32
	baseAnchor  time.Time // arrival of base packet
	lastPopped  uint32    // last extSeq released (or evicted as "played")
	hasLastPopd bool

	// Counters.
	pushed, popped                               uint64
	droppedLate, droppedOverflow, droppedExpired uint64
}

// NewJitterBuffer constructs a JitterBuffer with the given options.
func NewJitterBuffer(opts JBOptions) *JitterBuffer {
	if opts.TargetDelay <= 0 {
		opts.TargetDelay = DefaultTargetDelay
	}
	if opts.MaxLate <= 0 {
		opts.MaxLate = DefaultMaxLate
	}
	if opts.Ptime <= 0 {
		opts.Ptime = DefaultPtime
	}
	if opts.Capacity <= 0 {
		opts.Capacity = 64
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &JitterBuffer{
		target:  opts.TargetDelay,
		maxLate: opts.MaxLate,
		ptime:   opts.Ptime,
		cap:     opts.Capacity,
		now:     opts.Now,
	}
}

// Push inserts pkt into the buffer. Returns true if accepted, false if the
// packet was dropped (too late or evicted by capacity). The returned bool is
// mostly informational; callers do not need to react.
func (j *JitterBuffer) Push(pkt Packet) bool {
	j.mu.Lock()
	defer j.mu.Unlock()

	now := j.now()
	seq := pkt.Header.SequenceNumber

	var extSeq uint32
	if !j.set {
		// First packet ever. Anchor the playout clock here.
		j.set = true
		j.roc = 0
		j.lastSeen = seq
		extSeq = uint32(seq)
		j.baseExtSeq = extSeq
		j.baseAnchor = now
	} else {
		extSeq = j.extend(seq)
	}

	if j.hasLastPopd && !seqAfter(extSeq, j.lastPopped) {
		// Already played out (or watermark passed it). Drop.
		j.droppedLate++
		return false
	}
	// Also drop if the scheduled playout time is already past MaxLate. This
	// catches a straggler that arrives long after Pop would have advanced
	// the watermark were anyone draining.
	sched := j.scheduledFor(extSeq)
	if now.Sub(sched) > j.maxLate {
		j.droppedLate++
		return false
	}

	if j.cap > 0 && j.h.Len() >= j.cap {
		// Evict the head: it's the oldest scheduled and the closest to
		// being late. Counts as overflow, not "late", so the two failure
		// modes stay distinguishable in stats.
		ev := heap.Pop(&j.h).(*jbItem)
		j.droppedOverflow++
		j.lastPopped = ev.extSeq
		j.hasLastPopd = true
	}
	heap.Push(&j.h, &jbItem{extSeq: extSeq, sched: sched, pkt: pkt})
	j.pushed++
	return true
}

// Pop returns the next packet to play if its scheduled time has arrived.
// The boolean is false when the buffer is empty or the head is not yet due.
// Stragglers whose head-of-line packet is already > MaxLate past due are
// discarded so the consumer can keep up.
func (j *JitterBuffer) Pop() (Packet, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	now := j.now()
	for j.h.Len() > 0 {
		head := j.h[0]
		if now.Sub(head.sched) > j.maxLate {
			// Past the grace window. Drop and try the next packet.
			_ = heap.Pop(&j.h)
			j.lastPopped = head.extSeq
			j.hasLastPopd = true
			j.droppedExpired++
			continue
		}
		if now.Before(head.sched) {
			// Not yet due — hold for the next tick.
			return Packet{}, false
		}
		_ = heap.Pop(&j.h)
		j.lastPopped = head.extSeq
		j.hasLastPopd = true
		j.popped++
		return head.pkt, true
	}
	return Packet{}, false
}

// Depth returns the current number of buffered packets.
func (j *JitterBuffer) Depth() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.h.Len()
}

// Stats returns a snapshot of counters.
func (j *JitterBuffer) Stats() JBStats {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JBStats{
		Pushed:          j.pushed,
		Popped:          j.popped,
		DroppedLate:     j.droppedLate,
		DroppedOverflow: j.droppedOverflow,
		DroppedExpired:  j.droppedExpired,
		Depth:           j.h.Len(),
	}
}

// extend converts a 16-bit RTP sequence into a 32-bit extended sequence,
// advancing the rollover counter on wrap. Caller must hold j.mu.
func (j *JitterBuffer) extend(seq uint16) uint32 {
	// Compare against the highest 16-bit seq observed. If the new value is
	// "ahead" but numerically lower, treat it as a wrap. If it's "behind"
	// the watermark by more than 0x8000 in raw distance, treat as a late
	// packet from the previous epoch.
	diff := int32(seq) - int32(j.lastSeen)
	switch {
	case diff > 0x7FFF: // late packet from a prior wrap
		// Came from the previous roc epoch.
		if j.roc > 0 {
			return (j.roc-1)<<16 | uint32(seq)
		}
		// Pre-base wrap with roc=0 isn't representable; clamp to 0.
		return uint32(seq)
	case diff < -0x7FFF: // forward wrap
		j.roc++
		j.lastSeen = seq
		return j.roc<<16 | uint32(seq)
	default:
		if diff > 0 {
			j.lastSeen = seq
		}
		return j.roc<<16 | uint32(seq)
	}
}

// scheduledFor returns the playout time for an extended sequence.
func (j *JitterBuffer) scheduledFor(extSeq uint32) time.Time {
	delta := int64(extSeq) - int64(j.baseExtSeq)
	return j.baseAnchor.Add(j.target + time.Duration(delta)*j.ptime)
}

// seqAfter reports whether a is strictly after b in 32-bit extended-seq
// space. Identity returns false.
func seqAfter(a, b uint32) bool { return a > b }

// --- heap ---

type jbItem struct {
	extSeq uint32
	sched  time.Time
	pkt    Packet
}

type jbHeap []*jbItem

func (h jbHeap) Len() int           { return len(h) }
func (h jbHeap) Less(i, j int) bool { return h[i].extSeq < h[j].extSeq }
func (h jbHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *jbHeap) Push(x any) { *h = append(*h, x.(*jbItem)) }
func (h *jbHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}
