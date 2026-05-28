package rtp

import (
	"errors"
	"fmt"
	"time"
)

const (
	// DTMFPayloadType is the SDP-negotiated payload type for RFC 2833
	// (RFC 4733) telephone events. Bellerophon advertises 101 in every
	// SDP answer so we treat it as fixed.
	DTMFPayloadType = 101
)

// DTMFEvent is one fully-formed keypress observed on the RTP stream. The
// Digit field is the ASCII representation ('0'-'9', '*', '#', 'A'-'D').
type DTMFEvent struct {
	Digit    byte          // ASCII representation
	Duration time.Duration // total observed duration (sender-reported)
	Volume   uint8         // power level in -dBm0, 0-63 (0 = strongest)
}

// rfc2833Payload is the parsed 4-byte wire form of one telephone-event packet.
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|     event     |E|R| volume    |          duration             |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
type rfc2833Payload struct {
	Event         byte
	End           bool
	Reserved      bool // R bit; should be 0 per spec but we don't reject
	Volume        uint8
	DurationTicks uint16
}

// parseRFC2833 decodes a single 4-byte telephone-event payload. Larger
// payloads are accepted with only the first 4 bytes used (some implementations
// pad). Shorter is rejected.
func parseRFC2833(payload []byte) (rfc2833Payload, error) {
	if len(payload) < 4 {
		return rfc2833Payload{}, fmt.Errorf("rfc2833: payload too short: %d (need 4)", len(payload))
	}
	return rfc2833Payload{
		Event:         payload[0],
		End:           payload[1]&0x80 != 0,
		Reserved:      payload[1]&0x40 != 0,
		Volume:        payload[1] & 0x3F,
		DurationTicks: uint16(payload[2])<<8 | uint16(payload[3]),
	}, nil
}

// errNonDTMFEvent is returned when an RFC 2833 event code is in the
// telephone-tone range (16-255) rather than a DTMF digit.
var errNonDTMFEvent = errors.New("rfc2833: event is not a DTMF digit")

// eventToDigit maps an RFC 2833 event code to its ASCII representation.
// Returns errNonDTMFEvent for tone events (16+) or any other unmapped value.
func eventToDigit(event byte) (byte, error) {
	switch {
	case event <= 9:
		return '0' + event, nil
	case event == 10:
		return '*', nil
	case event == 11:
		return '#', nil
	case event >= 12 && event <= 15:
		return 'A' + (event - 12), nil
	default:
		return 0, errNonDTMFEvent
	}
}

// ticksToDuration converts an RFC 2833 duration field (in RTP-clock ticks)
// to a time.Duration given the clock rate in Hz. For telephone-event/8000
// the clock is 8000.
func ticksToDuration(ticks uint16, clockRateHz uint32) time.Duration {
	if clockRateHz == 0 {
		return 0
	}
	return time.Duration(int64(ticks)) * time.Second / time.Duration(clockRateHz)
}
