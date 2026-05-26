package sipua

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SDP payload type numbers for the codecs M001 negotiates. Telephone-event
// (RFC 2833 DTMF) is dynamic; we pin it to 101 to match the v1 stack and
// most softswitches' defaults.
const (
	PayloadPCMU          uint8 = 0
	PayloadPCMA          uint8 = 8
	PayloadTelephoneEvt  uint8 = 101
	DefaultPtimeMs             = 20
	DefaultMediaRTPProto       = "RTP/AVP"
)

// SDP is the slice of a session description we care about for M001 — just
// enough to negotiate an audio session. Lines we don't use are ignored.
type SDP struct {
	// ConnectionIP is the c= line address ("IN IP4 a.b.c.d").
	ConnectionIP string
	// Audio is the m=audio media section. Only the first audio media block
	// is parsed; M001 doesn't negotiate multi-stream.
	Audio AudioMedia
}

// AudioMedia is the parsed view of an m=audio block plus its a= attributes.
type AudioMedia struct {
	Port        int
	Proto       string
	PayloadList []uint8
	// MediaIP overrides ConnectionIP if the m-block carries its own c=
	// line (RFC 4566 §5.7). Empty if absent.
	MediaIP string
	// PtimeMs is the negotiated packetisation time from a=ptime, or 0 if
	// unspecified. Bellerophon assumes 20 ms when absent.
	PtimeMs int
	// Direction is sendrecv/sendonly/recvonly/inactive. Empty if unset
	// (defaults to sendrecv per RFC 4566 §6).
	Direction string
}

// ParseSDP parses the subset of SDP that M001 needs. It is permissive —
// unknown lines are skipped, line endings may be CRLF or LF — but it
// rejects malformed m= or c= lines.
//
// Only the first m=audio block is captured; subsequent media sections are
// ignored.
func ParseSDP(body []byte) (*SDP, error) {
	if len(body) == 0 {
		return nil, errors.New("sdp: empty body")
	}
	out := &SDP{}
	var (
		inAudio    bool
		audioSeen  bool
		sessionC   string
		mediaC     string
		ptime      int
		direction  string
	)
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimRight(string(raw), "\r")
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		field, value := line[0], strings.TrimSpace(line[2:])
		switch field {
		case 'c':
			if inAudio {
				mediaC = value
			} else {
				sessionC = value
			}
		case 'm':
			// Stop capturing attributes once a second media block starts.
			if audioSeen {
				inAudio = false
				continue
			}
			if !strings.HasPrefix(value, "audio ") {
				inAudio = false
				continue
			}
			port, payloads, proto, err := parseAudioM(value)
			if err != nil {
				return nil, err
			}
			out.Audio.Port = port
			out.Audio.Proto = proto
			out.Audio.PayloadList = payloads
			inAudio = true
			audioSeen = true
		case 'a':
			if !inAudio {
				continue
			}
			switch {
			case strings.HasPrefix(value, "ptime:"):
				v, err := strconv.Atoi(strings.TrimSpace(value[len("ptime:"):]))
				if err == nil && v > 0 {
					ptime = v
				}
			case value == "sendrecv", value == "sendonly", value == "recvonly", value == "inactive":
				direction = value
			}
		}
	}
	if !audioSeen {
		return nil, errors.New("sdp: no m=audio media block")
	}
	if sessionC != "" {
		ip, err := parseConnectionAddr(sessionC)
		if err != nil {
			return nil, err
		}
		out.ConnectionIP = ip
	}
	if mediaC != "" {
		ip, err := parseConnectionAddr(mediaC)
		if err != nil {
			return nil, err
		}
		out.Audio.MediaIP = ip
	}
	if out.ConnectionIP == "" && out.Audio.MediaIP == "" {
		return nil, errors.New("sdp: missing c= line")
	}
	out.Audio.PtimeMs = ptime
	out.Audio.Direction = direction
	return out, nil
}

// RemoteAudioAddr returns the IPv4 host:port the remote party expects RTP on.
// Media-level c= takes precedence over session-level c= per RFC 4566.
func (s *SDP) RemoteAudioAddr() string {
	ip := s.Audio.MediaIP
	if ip == "" {
		ip = s.ConnectionIP
	}
	return fmt.Sprintf("%s:%d", ip, s.Audio.Port)
}

// SelectCodec picks the first codec from supported that also appears in the
// remote offer's payload list. Returns (codec, true) on match, (0, false)
// otherwise. supported is in our preference order.
func (s *SDP) SelectCodec(supported []uint8) (uint8, bool) {
	have := map[uint8]bool{}
	for _, p := range s.Audio.PayloadList {
		have[p] = true
	}
	for _, pt := range supported {
		if have[pt] {
			return pt, true
		}
	}
	return 0, false
}

// SupportsTelephoneEvent reports whether the remote offered RFC 2833 DTMF
// on payload type 101. Bellerophon doesn't renegotiate the DTMF PT to a
// different dynamic value — we expect 101 or we omit it from the answer.
func (s *SDP) SupportsTelephoneEvent() bool {
	for _, p := range s.Audio.PayloadList {
		if p == PayloadTelephoneEvt {
			return true
		}
	}
	return false
}

// AnswerOptions configures BuildAnswer.
type AnswerOptions struct {
	// LocalIP is the IPv4 we advertise in c=. Required.
	LocalIP string
	// LocalRTPPort is the UDP port the remote should send RTP to. Required.
	LocalRTPPort int
	// Codec is the negotiated audio payload type (PCMU or PCMA).
	Codec uint8
	// IncludeTelephoneEvent appends payload 101 / RFC 2833 to the answer.
	IncludeTelephoneEvent bool
	// SessionUser is the username in o= (default "bellerophon").
	SessionUser string
	// SessionID is the session-id in o=. If 0, a deterministic value
	// derived from LocalRTPPort is used; callers wiring real calls should
	// pass a monotonic id (e.g. unix-nanos).
	SessionID uint64
	// SessionName is the s= line value (default "Bellerophon").
	SessionName string
}

// BuildAnswer renders an SDP body suitable for a 200 OK response to an
// INVITE. The answer advertises a single audio media stream with the
// negotiated codec, ptime:20, and sendrecv.
func BuildAnswer(opts AnswerOptions) ([]byte, error) {
	if opts.LocalIP == "" {
		return nil, errors.New("sdp: LocalIP required")
	}
	if opts.LocalRTPPort <= 0 || opts.LocalRTPPort > 65535 {
		return nil, fmt.Errorf("sdp: LocalRTPPort %d out of range", opts.LocalRTPPort)
	}
	if opts.Codec != PayloadPCMU && opts.Codec != PayloadPCMA {
		return nil, fmt.Errorf("sdp: unsupported codec %d", opts.Codec)
	}
	user := opts.SessionUser
	if user == "" {
		user = "bellerophon"
	}
	name := opts.SessionName
	if name == "" {
		name = "Bellerophon"
	}
	sid := opts.SessionID
	if sid == 0 {
		sid = uint64(opts.LocalRTPPort)
	}

	codecName := "PCMU"
	if opts.Codec == PayloadPCMA {
		codecName = "PCMA"
	}

	payloads := strconv.Itoa(int(opts.Codec))
	if opts.IncludeTelephoneEvent {
		payloads += " " + strconv.Itoa(int(PayloadTelephoneEvt))
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "v=0\r\n")
	fmt.Fprintf(&b, "o=%s %d %d IN IP4 %s\r\n", user, sid, sid, opts.LocalIP)
	fmt.Fprintf(&b, "s=%s\r\n", name)
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", opts.LocalIP)
	fmt.Fprintf(&b, "t=0 0\r\n")
	fmt.Fprintf(&b, "m=audio %d %s %s\r\n", opts.LocalRTPPort, DefaultMediaRTPProto, payloads)
	fmt.Fprintf(&b, "a=rtpmap:%d %s/8000\r\n", opts.Codec, codecName)
	if opts.IncludeTelephoneEvent {
		fmt.Fprintf(&b, "a=rtpmap:%d telephone-event/8000\r\n", PayloadTelephoneEvt)
		fmt.Fprintf(&b, "a=fmtp:%d 0-16\r\n", PayloadTelephoneEvt)
	}
	fmt.Fprintf(&b, "a=ptime:%d\r\n", DefaultPtimeMs)
	fmt.Fprintf(&b, "a=sendrecv\r\n")
	return b.Bytes(), nil
}

// NegotiateAnswer is the convenience pipeline: parse the remote offer, pick
// a codec from our preference order, and render an answer with our local
// RTP endpoint. Returns the answer body and the chosen codec.
//
// Returns an error if the offer is malformed or has no codec we support.
func NegotiateAnswer(offer []byte, localIP string, localPort int, preferred []uint8) ([]byte, uint8, *SDP, error) {
	parsed, err := ParseSDP(offer)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("parse offer: %w", err)
	}
	if len(preferred) == 0 {
		preferred = []uint8{PayloadPCMU, PayloadPCMA}
	}
	codec, ok := parsed.SelectCodec(preferred)
	if !ok {
		return nil, 0, parsed, fmt.Errorf("no supported codec in offer (got %v)", parsed.Audio.PayloadList)
	}
	body, err := BuildAnswer(AnswerOptions{
		LocalIP:               localIP,
		LocalRTPPort:          localPort,
		Codec:                 codec,
		IncludeTelephoneEvent: parsed.SupportsTelephoneEvent(),
	})
	if err != nil {
		return nil, 0, parsed, err
	}
	return body, codec, parsed, nil
}

// parseAudioM parses an "audio <port> <proto> <pt> [<pt>...]" line.
func parseAudioM(value string) (int, []uint8, string, error) {
	fields := strings.Fields(value)
	if len(fields) < 4 || fields[0] != "audio" {
		return 0, nil, "", fmt.Errorf("sdp: malformed m=audio line %q", value)
	}
	port, err := strconv.Atoi(fields[1])
	if err != nil || port <= 0 || port > 65535 {
		return 0, nil, "", fmt.Errorf("sdp: invalid audio port %q", fields[1])
	}
	proto := fields[2]
	pts := make([]uint8, 0, len(fields)-3)
	for _, p := range fields[3:] {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 127 {
			return 0, nil, "", fmt.Errorf("sdp: invalid payload type %q", p)
		}
		pts = append(pts, uint8(v))
	}
	return port, pts, proto, nil
}

// parseConnectionAddr extracts the host from a "c=" value like
// "IN IP4 192.168.1.10". M001 is IPv4-only.
func parseConnectionAddr(value string) (string, error) {
	fields := strings.Fields(value)
	if len(fields) < 3 || fields[0] != "IN" {
		return "", fmt.Errorf("sdp: malformed c= line %q", value)
	}
	if fields[1] != "IP4" {
		return "", fmt.Errorf("sdp: non-IPv4 c= line %q (M001 is IPv4 only)", value)
	}
	// The address may carry a "/ttl" or "/ttl/count" suffix for multicast;
	// strip it.
	host := fields[2]
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return "", fmt.Errorf("sdp: empty address in c= line %q", value)
	}
	return host, nil
}
