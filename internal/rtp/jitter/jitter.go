// Jitter buffer for inbound RTP. Reorders packets by RTP sequence number
// (handling 16-bit wraparound) and releases them on a steady playout clock
// so the codec consumer sees a smooth stream even when the network jitters.
package jitter

import (
	"container/heap"
	"sync"
	"time"

	rtp "github.com/stefandsl/bellerophon-go/internal/rtp"
)

const (
	DefaultTargetDelay = 60 * time.Millisecond
	DefaultMaxLate     = 100 * time.Millisecond
	DefaultPtime       = 20 * time.Millisecond
)

// JBOptions configures a JitterBuffer.
type JBOptions struct {
	TargetDelay time.Duration
	MaxLate     time.Duration
	Ptime       time.Duration
	Capacity    int
	Now         func() time.Time
}

// JBStats is a snapshot of buffer counters.
type JBStats struct {
	Pushed          uint64
	Popped          uint64
	DroppedLate     uint64
	DroppedOverflow uint64
	DroppedExpired  uint64
	Depth           int
}

// JitterBuffer reorders RTP packets and releases them on a steady clock.
type JitterBuffer struct {
	target  time.Duration
	maxLate time.Duration
	ptime   time.Duration
	cap     int
	now     func() time.Time

	mu sync.Mutex
	h  jbHeap

	set         bool
	roc         uint32
	lastSeen    uint16
	baseExtSeq  uint32
	baseAnchor  time.Time
	lastPopped  uint32
	hasLastPopd bool

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
// packet was dropped (too late or evicted by capacity).
func (j *JitterBuffer) Push(pkt rtp.Packet) bool {
	j.mu.Lock()
	defer j.mu.Unlock()

	now := j.now()
	seq := pkt.Header.SequenceNumber

	var extSeq uint32
	if !j.set {
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
		j.droppedLate++
		return false
	}
	sched := j.scheduledFor(extSeq)
	if now.Sub(sched) > j.maxLate {
		j.droppedLate++
		return false
	}

	if j.cap > 0 && j.h.Len() >= j.cap {
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
func (j *JitterBuffer) Pop() (rtp.Packet, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	now := j.now()
	for j.h.Len() > 0 {
		head := j.h[0]
		if now.Sub(head.sched) > j.maxLate {
			_ = heap.Pop(&j.h)
			j.lastPopped = head.extSeq
			j.hasLastPopd = true
			j.droppedExpired++
			continue
		}
		if now.Before(head.sched) {
			return rtp.Packet{}, false
		}
		_ = heap.Pop(&j.h)
		j.lastPopped = head.extSeq
		j.hasLastPopd = true
		j.popped++
		return head.pkt, true
	}
	return rtp.Packet{}, false
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

func (j *JitterBuffer) extend(seq uint16) uint32 {
	diff := int32(seq) - int32(j.lastSeen)
	switch {
	case diff > 0x7FFF:
		if j.roc > 0 {
			return (j.roc-1)<<16 | uint32(seq)
		}
		return uint32(seq)
	case diff < -0x7FFF:
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

func (j *JitterBuffer) scheduledFor(extSeq uint32) time.Time {
	delta := int64(extSeq) - int64(j.baseExtSeq)
	return j.baseAnchor.Add(j.target + time.Duration(delta)*j.ptime)
}

func seqAfter(a, b uint32) bool { return a > b }

// --- heap ---

type jbItem struct {
	extSeq uint32
	sched  time.Time
	pkt    rtp.Packet
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
