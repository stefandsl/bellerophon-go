package conversation

import "strings"

// Strings is the per-language bag of canned phrases the loop falls
// back to when it has to speak something the LLM didn't generate —
// greetings, "I didn't hear you", error fallbacks, goodbye line. It
// is a minimal port of voice-app/lib/voice-strings.js scoped to the
// keys conversation-loop.js actually consumed. The multi-language
// DTMF picker, voice-fallback table, picker-half URLs and thinking
// phrases all live in voice-app today; they will land in M003 when
// the language picker reappears.
type Strings struct {
	// Greeting is played once at the start of the call.
	Greeting string
	// NoSpeech is played when LISTENING times out without an utterance.
	NoSpeech string
	// Clarify is played when the transcript is empty or too short to
	// be a real turn.
	Clarify string
	// Goodbye is played right before HANGUP when the caller said one
	// of the goodbye keywords.
	Goodbye string
	// MaxTurns is played when MaxTurns is reached.
	MaxTurns string
	// Error is played in the catch-all error path.
	Error string
	// BridgeUnknown is the LLM-error fallback. Spoken in the agent's
	// own voice so an LLM outage doesn't break the language illusion
	// for non-English callers.
	BridgeUnknown string
}

// DefaultEnglishStrings is voice-app's en bag. Word-for-word port so
// regression tests can compare audio byte-for-byte against the Node
// stack on the same ElevenLabs voice.
func DefaultEnglishStrings() Strings {
	return Strings{
		Greeting:      "Hello! I'm Aïtheia. How can I help you today?",
		NoSpeech:      "I didn't hear anything. Are you still there?",
		Clarify:       "Sorry, I didn't catch that. Could you repeat?",
		Goodbye:       "Goodbye! Call again anytime.",
		MaxTurns:      "We've been talking for a while. Goodbye!",
		Error:         "Sorry, something went wrong.",
		BridgeUnknown: "Sorry, I ran into an unexpected error. One moment, please.",
	}
}

// DefaultItalianStrings is voice-app's it bag. Kept here rather than
// in a separate file so adding/checking translations doesn't require
// hopping between files. M002 ships en+it; further locales are M003.
func DefaultItalianStrings() Strings {
	return Strings{
		Greeting:      "Ciao! Sono Aïtheia. Come posso aiutarti oggi?",
		NoSpeech:      "Non ho sentito nulla. Sei ancora lì?",
		Clarify:       "Scusa, non ho capito. Puoi ripetere?",
		Goodbye:       "Arrivederci! Richiamami quando vuoi.",
		MaxTurns:      "Abbiamo parlato per un bel po'. Arrivederci!",
		Error:         "Scusa, qualcosa è andato storto.",
		BridgeUnknown: "Scusa, ho riscontrato un errore inatteso. Un momento per favore.",
	}
}

// goodbyeKeywords is the union of en+it goodbye phrases voice-app
// uses. The list is a defence-in-depth measure — the LLM-side
// directive ("respond in $LANG") prevents most ambiguity, but a
// caller saying "ciao" mid-conversation in an Italian session should
// still end the call regardless of which agent persona is loaded.
var goodbyeKeywords = []string{
	// English
	"goodbye", "good bye", "bye", "hang up", "end call", "that's all", "thats all",
	// Italian
	"arrivederci", "ciao", "a presto", "riattacca", "chiudi",
	"basta cosi", "basta così", "è tutto", "e tutto", "fine chiamata", "a dopo",
}

// IsGoodbye reports whether transcript looks like a hangup request.
// Matches voice-app's permissive policy: whole-string match, or the
// keyword bracketed by spaces / at a string boundary. Sub-string-only
// matches inside a word ("byeway") don't count. Trailing/leading
// punctuation is stripped before comparison so "Goodbye!" matches
// the same as "goodbye" — Whisper routinely emits sentence-final
// punctuation that voice-app's JS regex never had to handle.
func IsGoodbye(transcript string) bool {
	lower := strings.ToLower(strings.TrimSpace(transcript))
	// Strip outer punctuation so "Goodbye!" / "Bye." / "Ciao!" hit.
	lower = strings.TrimFunc(lower, isPunctOrSpace)
	if lower == "" {
		return false
	}
	for _, kw := range goodbyeKeywords {
		if lower == kw ||
			hasWordBoundaryPrefix(lower, kw) ||
			hasWordBoundarySuffix(lower, kw) ||
			containsWord(lower, kw) {
			return true
		}
	}
	return false
}

// isPunctOrSpace matches typical sentence-final punctuation Whisper
// emits and is forgiving of accented variants ("¡adios!"). Anything
// not a letter or digit is fair game — voice transcripts never use
// punctuation as part of an identifier.
func isPunctOrSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r',
		'.', ',', '!', '?', ';', ':',
		'"', '\'', '`', '(', ')', '[', ']', '{', '}',
		'¡', '¿':
		return true
	}
	return false
}

// hasWordBoundaryPrefix is `lower.startsWith(kw + " ")` made
// punctuation-tolerant: "bye," and "bye!" both match.
func hasWordBoundaryPrefix(lower, kw string) bool {
	if !strings.HasPrefix(lower, kw) {
		return false
	}
	if len(lower) == len(kw) {
		return true
	}
	return isPunctOrSpace(rune(lower[len(kw)]))
}

func hasWordBoundarySuffix(lower, kw string) bool {
	if !strings.HasSuffix(lower, kw) {
		return false
	}
	if len(lower) == len(kw) {
		return true
	}
	return isPunctOrSpace(rune(lower[len(lower)-len(kw)-1]))
}

func containsWord(lower, kw string) bool {
	idx := strings.Index(lower, kw)
	if idx <= 0 {
		return false
	}
	if !isPunctOrSpace(rune(lower[idx-1])) {
		return false
	}
	end := idx + len(kw)
	if end >= len(lower) {
		return true
	}
	return isPunctOrSpace(rune(lower[end]))
}
