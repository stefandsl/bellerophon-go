package conversation

import "testing"

func TestIsGoodbye_PositiveCases(t *testing.T) {
	t.Parallel()
	cases := []string{
		"goodbye",
		"Goodbye!",
		"good bye",
		"bye",
		"  Bye  ",
		"OK, bye",
		"thats all for now",
		"that's all thanks",
		"arrivederci",
		"Arrivederci a presto",
		"ciao",
		"ciao ciao",
		"basta cosi",
		"basta così",
		"è tutto",
		"fine chiamata",
	}
	for _, in := range cases {
		if !IsGoodbye(in) {
			t.Errorf("IsGoodbye(%q) = false, want true", in)
		}
	}
}

func TestIsGoodbye_NegativeCases(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"hello",
		"how are you",
		"byeway",            // substring inside a word shouldn't match
		"goodnight",         // distinct word
		"ciaone",            // substring inside a word
		"buongiorno",        // unrelated Italian
		"thats a llama bye", // last word IS bye → match — sanity check inverse
	}
	for _, in := range cases[:len(cases)-1] {
		if IsGoodbye(in) {
			t.Errorf("IsGoodbye(%q) = true, want false", in)
		}
	}
	// Sanity check: trailing " bye" still matches (that's intentional).
	if !IsGoodbye(cases[len(cases)-1]) {
		t.Errorf("IsGoodbye(%q) should match trailing 'bye'", cases[len(cases)-1])
	}
}

func TestDefaultEnglishStrings_AllNonEmpty(t *testing.T) {
	t.Parallel()
	s := DefaultEnglishStrings()
	allFields := []struct {
		name string
		val  string
	}{
		{"Greeting", s.Greeting},
		{"NoSpeech", s.NoSpeech},
		{"Clarify", s.Clarify},
		{"Goodbye", s.Goodbye},
		{"MaxTurns", s.MaxTurns},
		{"Error", s.Error},
		{"BridgeUnknown", s.BridgeUnknown},
	}
	for _, f := range allFields {
		if f.val == "" {
			t.Errorf("Strings.%s is empty", f.name)
		}
	}
}

func TestDefaultItalianStrings_AllNonEmpty(t *testing.T) {
	t.Parallel()
	s := DefaultItalianStrings()
	if s.Greeting == "" || s.Goodbye == "" || s.BridgeUnknown == "" {
		t.Errorf("Italian strings missing critical fields: %+v", s)
	}
}
