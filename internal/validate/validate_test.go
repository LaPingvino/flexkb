package validate

import (
	"testing"
)

// TestDecodeUEscape — codepoint parsing for compose-chain indexing.
func TestDecodeUEscape(t *testing.T) {
	cases := []struct {
		in   string
		want rune
		ok   bool
	}{
		{"U0915", 0x0915, true},
		{"u00fc", 0x00fc, true},
		{"U0", 0, true},
		{"", 0, false},
		{"0x0915", 0, false}, // wrong prefix
		{"Uzzzz", 0, false},  // bad hex
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, ok := decodeUEscape(c.in)
			if ok != c.ok || (ok && got != c.want) {
				t.Errorf("got (%U, %v), want (%U, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

// TestRuneOfSingleChar — coverage.Analyze emits missing chars as
// single-rune strings; runeOf must extract the rune unchanged.
func TestRuneOfSingleChar(t *testing.T) {
	if r := runeOf("ç"); r != 'ç' {
		t.Errorf("got %U, want %U", r, 'ç')
	}
	if r := runeOf(""); r != 0 {
		t.Errorf("empty string: got %U, want 0", r)
	}
}

// TestSuggestionIndexDedupsByLayer — a layer that produces the
// same character via multiple positions/levels is listed once,
// not N times.
func TestSuggestionIndexDedupsByLayer(t *testing.T) {
	idx := &suggestionIndex{byChar: map[rune][]Suggestion{}}
	sug := Suggestion{Name: "intl", Kind: "addition"}
	idx.add('ñ', sug)
	idx.add('ñ', sug)
	idx.add('ñ', sug)
	if got := idx.byChar['ñ']; len(got) != 1 {
		t.Errorf("got %d entries for ñ, want 1", len(got))
	}
}

// TestSuggestionIndexExcludesAlreadyPresent — when a recipe
// already lists "intl" as an addition, ñ is not suggested via
// intl again; the user wants OTHER paths to the missing char.
func TestSuggestionIndexExcludesAlreadyPresent(t *testing.T) {
	idx := &suggestionIndex{byChar: map[rune][]Suggestion{}}
	idx.add('ñ', Suggestion{Name: "intl", Kind: "addition"})
	idx.add('ñ', Suggestion{Name: "esperanto", Kind: "addition"})

	exclude := map[layerKey]bool{
		{Kind: "addition", Name: "intl"}: true,
	}
	got := idx.suggestionsFor('ñ', exclude)
	if len(got) != 1 || got[0].Name != "esperanto" {
		t.Errorf("got %+v, want [esperanto]", got)
	}
}

// TestFormatReport — pretty-printer stable output (the GUI/CLI
// both render via this so a regression is user-visible).
func TestFormatReport(t *testing.T) {
	r := Report{
		File: "be", Variant: "basic", Locale: "fr",
		Coverage: 0.95, Total: 40,
		Missing: []MissingItem{
			{Char: "Ç", Suggestions: []Suggestion{
				{Name: "polish-on-intl", Kind: "addition"},
				{Name: "french-accents", Kind: "addition"},
			}},
			{Char: "œ", Suggestions: nil},
		},
	}
	got := FormatReport(r)
	// Not asserting exact bytes — just that key fields are present
	// in the user-visible string.
	for _, want := range []string{
		"be(basic)", "locale fr", "Ç", "polish-on-intl", "french-accents",
		"œ", "no layer in data/ produces this",
	} {
		if !contains(got, want) {
			t.Errorf("expected %q in formatted report:\n%s", want, got)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
