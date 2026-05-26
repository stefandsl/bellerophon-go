package sipua

import (
	"strings"
	"testing"
)

const linphoneOffer = `v=0
o=linphone 4096 4096 IN IP4 192.168.1.42
s=Talk
c=IN IP4 192.168.1.42
t=0 0
m=audio 7078 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
`

const pcmaOnlyOffer = "v=0\r\n" +
	"o=- 1 1 IN IP4 10.0.0.5\r\n" +
	"s=-\r\n" +
	"c=IN IP4 10.0.0.5\r\n" +
	"t=0 0\r\n" +
	"m=audio 30000 RTP/AVP 8\r\n" +
	"a=rtpmap:8 PCMA/8000\r\n"

const opusOnlyOffer = `v=0
o=- 1 1 IN IP4 10.0.0.5
s=-
c=IN IP4 10.0.0.5
t=0 0
m=audio 30000 RTP/AVP 96
a=rtpmap:96 opus/48000/2
`

const mediaLevelCOffer = `v=0
o=- 1 1 IN IP4 192.0.2.1
s=-
t=0 0
m=audio 40000 RTP/AVP 0
c=IN IP4 198.51.100.7
a=rtpmap:0 PCMU/8000
`

func TestParseSDP_Linphone(t *testing.T) {
	s, err := ParseSDP([]byte(linphoneOffer))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if s.ConnectionIP != "192.168.1.42" {
		t.Errorf("ConnectionIP = %q, want 192.168.1.42", s.ConnectionIP)
	}
	if s.Audio.Port != 7078 {
		t.Errorf("Port = %d, want 7078", s.Audio.Port)
	}
	if s.Audio.Proto != "RTP/AVP" {
		t.Errorf("Proto = %q, want RTP/AVP", s.Audio.Proto)
	}
	wantPTs := []uint8{0, 8, 101}
	if len(s.Audio.PayloadList) != len(wantPTs) {
		t.Fatalf("PayloadList = %v, want %v", s.Audio.PayloadList, wantPTs)
	}
	for i, p := range wantPTs {
		if s.Audio.PayloadList[i] != p {
			t.Errorf("PayloadList[%d] = %d, want %d", i, s.Audio.PayloadList[i], p)
		}
	}
	if s.Audio.PtimeMs != 20 {
		t.Errorf("PtimeMs = %d, want 20", s.Audio.PtimeMs)
	}
	if s.Audio.Direction != "sendrecv" {
		t.Errorf("Direction = %q, want sendrecv", s.Audio.Direction)
	}
	if got := s.RemoteAudioAddr(); got != "192.168.1.42:7078" {
		t.Errorf("RemoteAudioAddr = %q", got)
	}
	if !s.SupportsTelephoneEvent() {
		t.Error("SupportsTelephoneEvent = false, want true")
	}
}

func TestParseSDP_MediaLevelConnectionWins(t *testing.T) {
	s, err := ParseSDP([]byte(mediaLevelCOffer))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if s.Audio.MediaIP != "198.51.100.7" {
		t.Errorf("Audio.MediaIP = %q, want 198.51.100.7", s.Audio.MediaIP)
	}
	if got := s.RemoteAudioAddr(); got != "198.51.100.7:40000" {
		t.Errorf("RemoteAudioAddr = %q, want media-level addr", got)
	}
}

func TestParseSDP_Errors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty", "", "empty body"},
		{"no audio", "v=0\r\nc=IN IP4 1.2.3.4\r\nt=0 0\r\n", "no m=audio"},
		{"no c=", "v=0\r\nm=audio 30000 RTP/AVP 0\r\n", "missing c="},
		{"bad port", "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 99999 RTP/AVP 0\r\n", "invalid audio port"},
		{"bad pt", "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 30000 RTP/AVP xyz\r\n", "invalid payload type"},
		{"ipv6 c=", "v=0\r\nc=IN IP6 ::1\r\nm=audio 30000 RTP/AVP 0\r\n", "non-IPv4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSDP([]byte(tc.body))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%q want substring %q", err, tc.want)
			}
		})
	}
}

func TestSelectCodec(t *testing.T) {
	s, err := ParseSDP([]byte(linphoneOffer))
	if err != nil {
		t.Fatal(err)
	}
	// Preference order PCMU > PCMA picks PCMU.
	got, ok := s.SelectCodec([]uint8{PayloadPCMU, PayloadPCMA})
	if !ok || got != PayloadPCMU {
		t.Errorf("SelectCodec PCMU-first = %d,%v want 0,true", got, ok)
	}
	// Preference order PCMA > PCMU picks PCMA.
	got, ok = s.SelectCodec([]uint8{PayloadPCMA, PayloadPCMU})
	if !ok || got != PayloadPCMA {
		t.Errorf("SelectCodec PCMA-first = %d,%v want 8,true", got, ok)
	}
	// No overlap.
	opus, err := ParseSDP([]byte(opusOnlyOffer))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := opus.SelectCodec([]uint8{PayloadPCMU, PayloadPCMA}); ok {
		t.Error("SelectCodec opus-offer should not match PCMU/PCMA")
	}
}

func TestBuildAnswer_Shape(t *testing.T) {
	body, err := BuildAnswer(AnswerOptions{
		LocalIP:               "10.0.0.1",
		LocalRTPPort:          30002,
		Codec:                 PayloadPCMU,
		IncludeTelephoneEvent: true,
		SessionID:             424242,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"v=0\r\n",
		"o=bellerophon 424242 424242 IN IP4 10.0.0.1\r\n",
		"c=IN IP4 10.0.0.1\r\n",
		"m=audio 30002 RTP/AVP 0 101\r\n",
		"a=rtpmap:0 PCMU/8000\r\n",
		"a=rtpmap:101 telephone-event/8000\r\n",
		"a=fmtp:101 0-16\r\n",
		"a=ptime:20\r\n",
		"a=sendrecv\r\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("answer missing %q\n---\n%s", want, s)
		}
	}
	// CRLF terminated, no stray LF-only lines.
	if strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") {
		t.Errorf("answer has bare LF lines:\n%s", s)
	}
}

func TestBuildAnswer_PCMA_NoDTMF(t *testing.T) {
	body, err := BuildAnswer(AnswerOptions{
		LocalIP:      "10.0.0.1",
		LocalRTPPort: 30000,
		Codec:        PayloadPCMA,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "m=audio 30000 RTP/AVP 8\r\n") {
		t.Errorf("want PCMA-only m-line, got:\n%s", s)
	}
	if !strings.Contains(s, "a=rtpmap:8 PCMA/8000") {
		t.Errorf("want PCMA rtpmap, got:\n%s", s)
	}
	if strings.Contains(s, "telephone-event") {
		t.Errorf("did not request DTMF, but answer includes telephone-event:\n%s", s)
	}
}

func TestBuildAnswer_Errors(t *testing.T) {
	cases := []struct {
		name string
		opts AnswerOptions
		want string
	}{
		{"no ip", AnswerOptions{LocalRTPPort: 30000, Codec: PayloadPCMU}, "LocalIP required"},
		{"bad port", AnswerOptions{LocalIP: "1.1.1.1", LocalRTPPort: 0, Codec: PayloadPCMU}, "LocalRTPPort"},
		{"bad codec", AnswerOptions{LocalIP: "1.1.1.1", LocalRTPPort: 30000, Codec: 96}, "unsupported codec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildAnswer(tc.opts)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%q want substring %q", err, tc.want)
			}
		})
	}
}

func TestNegotiateAnswer_HappyPath(t *testing.T) {
	body, codec, parsed, err := NegotiateAnswer([]byte(linphoneOffer), "10.0.0.1", 30000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if codec != PayloadPCMU {
		t.Errorf("codec = %d, want PCMU", codec)
	}
	if parsed.RemoteAudioAddr() != "192.168.1.42:7078" {
		t.Errorf("parsed.RemoteAudioAddr = %q", parsed.RemoteAudioAddr())
	}
	if !strings.Contains(string(body), "m=audio 30000 RTP/AVP 0 101") {
		t.Errorf("answer wrong shape:\n%s", body)
	}
}

func TestNegotiateAnswer_PCMA_Only(t *testing.T) {
	body, codec, _, err := NegotiateAnswer([]byte(pcmaOnlyOffer), "10.0.0.1", 30000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if codec != PayloadPCMA {
		t.Errorf("codec = %d, want PCMA", codec)
	}
	if !strings.Contains(string(body), "m=audio 30000 RTP/AVP 8\r\n") {
		t.Errorf("answer must be PCMA-only:\n%s", body)
	}
}

func TestNegotiateAnswer_NoCodecOverlap(t *testing.T) {
	_, _, _, err := NegotiateAnswer([]byte(opusOnlyOffer), "10.0.0.1", 30000, nil)
	if err == nil {
		t.Fatal("expected error on opus-only offer")
	}
	if !strings.Contains(err.Error(), "no supported codec") {
		t.Errorf("err = %q", err)
	}
}
