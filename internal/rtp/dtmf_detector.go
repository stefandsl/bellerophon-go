package rtp

import (
	"sync"

	bellog "github.com/stefandsl/bellerophon-go/internal/log"
)

// DTMFDetectorOptions configures NewDTMFDetector.
type DTMFDetectorOptions struct {
	// ClockRate is the RTP timestamp clock for the negotiated
	// telephone-event PT. RFC 2833 reference is 8000 Hz; SDP can negotiate
	// other rates but we only ever advertise 8000. Zero defaults to 8000.
	ClockRate uint32
	// BufSize is the capacity of the Events() channel. Zero -> 16, enough
	// for sustained pressing without dropping while a consumer drains.
	BufSize int
	// Logger is optional; nil means silent.
	Logger bellog.Logger
}

// DTMFDetector parses RFC 2833 telephone-event packets fed via Push() and
// emits one DTMFEvent per keypress on Events(). Multiple end-bit packets
// (RFC 2833 mandates 3 retransmissions) are deduplicated by RTP timestamp.
//
// The detector is push-driven: the caller decides which packets reach it,
// typically based on PT == DTMFPayloadType. Keeping Session unaware of DTMF
// avoids a tight coupling between the wire layer and the event surface.
type DTMFDetector struct {
	clockRate uint32
	logger    bellog.Logger
	events    chan DTMFEvent

	mu            sync.Mutex
	hasEmitted    bool
	lastEmittedTS uint32
	closed        bool
}

// NewDTMFDetector builds a detector with the given options.
func NewDTMFDetector(opts DTMFDetectorOptions) *DTMFDetector {
	if opts.ClockRate == 0 {
		opts.ClockRate = 8000
	}
	bufSize := opts.BufSize
	if bufSize <= 0 {
		bufSize = 16
	}
	return &DTMFDetector{
		clockRate: opts.ClockRate,
		logger:    opts.Logger,
		events:    make(chan DTMFEvent, bufSize),
	}
}

// Events returns the channel on which detected keypresses arrive. The channel
// closes when Close is called.
func (d *DTMFDetector) Events() <-chan DTMFEvent { return d.events }

// Close closes the Events channel. Safe to call multiple times.
func (d *DTMFDetector) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	close(d.events)
}

// Push parses pkt as RFC 2833 and, if it is a DTMF end packet for a new
// keypress, emits a DTMFEvent. Non-DTMF events (telephone tones), parse
// errors, and non-end packets are silently absorbed.
//
// The dedup contract: at most one event is emitted per distinct RTP
// timestamp. The first end-bit packet for a given timestamp wins; retries
// of the same end packet are dropped.
func (d *DTMFDetector) Push(pkt Packet) {
	parsed, err := parseRFC2833(pkt.Payload)
	if err != nil {
		if d.logger != nil {
			d.logger.Debug("dtmf parse failed", "err", err.Error())
		}
		return
	}
	digit, err := eventToDigit(parsed.Event)
	if err != nil {
		// Non-DTMF tone (event ≥ 16) — silently ignore. The spec doesn't
		// require us to handle those for telephony.
		return
	}
	if !parsed.End {
		// Mid-press updates carry no extra information for our purposes;
		// we only emit on the end bit.
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	ts := pkt.Header.Timestamp
	if d.hasEmitted && ts == d.lastEmittedTS {
		// Redundant end packet — RFC 2833 mandates 3, we only emit on the first.
		return
	}

	ev := DTMFEvent{
		Digit:    digit,
		Duration: ticksToDuration(parsed.DurationTicks, d.clockRate),
		Volume:   parsed.Volume,
	}
	select {
	case d.events <- ev:
	default:
		if d.logger != nil {
			d.logger.Warn("dtmf channel full, dropping event",
				"digit", string(digit), "ts", ts)
		}
	}
	d.lastEmittedTS = ts
	d.hasEmitted = true
}
