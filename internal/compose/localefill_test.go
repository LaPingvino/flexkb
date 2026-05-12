package compose

import (
	"testing"

	"github.com/lapingvino/flexkb/internal/model"
)

// TestLocaleFillPlacesUppercaseWhenLowercaseAtL1 covers the Belgian
// AZERTY+intl case from 2026-05-12: nudge mode preserves the digit row
// but causes the intl cascade to drop Ccedilla off the end. locale-fill
// should rescue it by placing the keysym in an empty slot. Anchor
// scoring is what makes Ç win over score-0 chars like Â when both
// compete for the same first-empty key — without sorting, alphabetic
// iteration eats AE12 with Â before Ç can land.
func TestLocaleFillPlacesUppercaseWhenLowercaseAtL1(t *testing.T) {
	phys := model.Physical{Name: "az-mini", Keys: []string{
		"AE09", "AE10", "AE12", "AB03", "AB05", "AB07", "AC11",
	}}
	trans := model.Transformation{Name: "az", Keys: map[string]model.KeySymbols{
		"AE09": {Levels: []string{"ccedilla", "9", "leftsinglequotemark", "dead_breve"}},
		"AE10": {Levels: []string{"agrave", "0", "rightsinglequotemark", "dead_abovering"}},
		"AE12": {Levels: []string{"equal", "plus"}},
		"AB03": {Levels: []string{"c", "C", "copyright", "cent"}},
		"AB05": {Levels: []string{"b", "B"}},
		"AB07": {Levels: []string{"comma", "question", "less", "ccedilla"}},
		"AC11": {Levels: []string{"ugrave", "percent"}},
	}}
	r := ComposeFromParts(model.LayoutSpec{Name: "t"}, phys, trans, nil)

	// Simulate the post-applyLocaleFillFromSpec missing list for fr:
	// uppercase variants of L1-anchored lowercase chars (Ç, À, Ù) plus
	// some score-0 chars that would clobber the easy slots without
	// anchor sorting (Â, Ê).
	missing := []rune{'Â', 'Ê', 'Ç', 'À', 'Ù'}
	placed := ApplyLocaleFill(&r, missing, "fr")
	if placed < 3 {
		t.Fatalf("expected at least 3 placements (Ç, À, Ù), got %d. Layout: %v", placed, r.Layout.Symbols)
	}

	// The whole point of the regression: Ç must land somewhere.
	mustProduce := []rune{'Ç', 'À', 'Ù'}
	for _, want := range mustProduce {
		if !produces(r, want) {
			t.Errorf("expected %q on the layout after locale-fill; got %v", string(want), dumpLayout(r))
		}
	}
}

// TestLocaleFillNoWorkWhenAllCovered: empty missing list is a no-op.
func TestLocaleFillNoWorkWhenAllCovered(t *testing.T) {
	phys := model.Physical{Name: "tiny", Keys: []string{"AC01"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A"}},
	}}
	r := ComposeFromParts(model.LayoutSpec{Name: "t"}, phys, trans, nil)
	if placed := ApplyLocaleFill(&r, nil, "en"); placed != 0 {
		t.Errorf("nil missing should place 0, got %d", placed)
	}
}

// TestLocaleFillReportsWarningOnNoSlot: when every key is full to L4,
// the missing char should be reported in r.Warnings, not silently lost.
func TestLocaleFillReportsWarningOnNoSlot(t *testing.T) {
	phys := model.Physical{Name: "full", Keys: []string{"AC01"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A", "adiaeresis", "Adiaeresis"}},
	}}
	r := ComposeFromParts(model.LayoutSpec{Name: "t"}, phys, trans, nil)
	placed := ApplyLocaleFill(&r, []rune{'Ç'}, "fr")
	if placed != 0 {
		t.Errorf("no slot available, expected 0 placed, got %d", placed)
	}
	gotWarn := false
	for _, w := range r.Warnings {
		if contains(w, "no slot") && contains(w, "Ç") {
			gotWarn = true
		}
	}
	if !gotWarn {
		t.Errorf("expected a 'no slot for Ç' warning, got %v", r.Warnings)
	}
}

func produces(r Result, want rune) bool {
	for _, sym := range r.Layout.Symbols {
		for _, lv := range sym.Levels {
			if rn, ok := decodeForTest(lv); ok && rn == want {
				return true
			}
		}
	}
	return false
}

func dumpLayout(r Result) map[string][]string {
	out := map[string][]string{}
	for _, k := range r.Layout.Keys {
		out[k] = r.Layout.Symbols[k].Levels
	}
	return out
}

// decodeForTest mirrors the small subset of coverage.Decode the tests
// need without importing the coverage package (avoiding test-only
// reverse coupling).
func decodeForTest(token string) (rune, bool) {
	known := map[string]rune{
		"Ccedilla": 'Ç', "ccedilla": 'ç',
		"Agrave": 'À', "agrave": 'à',
		"Ugrave": 'Ù', "ugrave": 'ù',
		"Acircumflex": 'Â', "acircumflex": 'â',
		"Ecircumflex": 'Ê', "ecircumflex": 'ê',
	}
	if r, ok := known[token]; ok {
		return r, true
	}
	if len(token) == 1 {
		return []rune(token)[0], true
	}
	return 0, false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
