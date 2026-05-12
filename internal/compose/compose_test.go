package compose

import (
	"testing"

	"github.com/lapingvino/flexkb/internal/model"
)

func TestComposeAppliesTransformationThenAdditions(t *testing.T) {
	phys := model.Physical{Name: "tiny", Keys: []string{"AC01", "AC02"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A"}},
		"AC02": {Levels: []string{"s", "S"}},
	}}
	add := model.Addition{Name: "ogonek", Overlays: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"", "", "aogonek", "Aogonek"}},
	}}
	spec := model.LayoutSpec{Name: "test", Physical: "tiny", Transformation: "qw"}
	r := ComposeFromParts(spec, phys, trans, []model.Addition{add})
	got := r.Layout.Symbols["AC01"].Levels
	want := []string{"a", "A", "aogonek", "Aogonek"}
	if len(got) != len(want) {
		t.Fatalf("AC01 levels: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AC01[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
	// AC02 untouched.
	gotS := r.Layout.Symbols["AC02"].Levels
	if len(gotS) != 2 || gotS[0] != "s" || gotS[1] != "S" {
		t.Errorf("AC02 should be unchanged, got %v", gotS)
	}
}

func TestComposeDropsKeysNotOnPhysical(t *testing.T) {
	phys := model.Physical{Name: "tiny", Keys: []string{"AC01"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A"}},
		// XYZ1 is a fictional key — not in extensionKeys — so the composer
		// should drop it AND warn (likely a data typo).
		"XYZ1": {Levels: []string{"backslash", "bar"}},
	}}
	r := ComposeFromParts(model.LayoutSpec{Name: "t"}, phys, trans, nil)
	if _, ok := r.Layout.Symbols["XYZ1"]; ok {
		t.Errorf("XYZ1 should be dropped on tiny physical")
	}
	if len(r.Warnings) == 0 {
		t.Errorf("expected a warning for dropped unknown key")
	}
}

// LSGT is in extensionKeys: cross-physical transformations that define it
// should compose onto ANSI silently — that's the design, not a typo.
func TestComposeSilentForExtensionKeys(t *testing.T) {
	phys := model.Physical{Name: "ansi-tiny", Keys: []string{"AC01"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A"}},
		"LSGT": {Levels: []string{"backslash", "bar"}},
	}}
	r := ComposeFromParts(model.LayoutSpec{Name: "t"}, phys, trans, nil)
	if _, ok := r.Layout.Symbols["LSGT"]; ok {
		t.Errorf("LSGT should be dropped on ANSI physical")
	}
	if len(r.Warnings) != 0 {
		t.Errorf("LSGT on ANSI should compose silently, got warnings: %v", r.Warnings)
	}
}

func TestAdditionPassThroughEmptyLevels(t *testing.T) {
	got := mergeLevels([]string{"a", "A"}, []string{"", "", "aogonek", "Aogonek"})
	want := []string{"a", "A", "aogonek", "Aogonek"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestAdditionTrimsTrailingEmpties(t *testing.T) {
	got := mergeLevels([]string{"a", "A", "", ""}, []string{"a", "A"})
	if len(got) != 2 {
		t.Errorf("trailing empties should be trimmed: got %v", got)
	}
}

// TestLetterOverlayFallback: if the primary letter isn't at level 1 of any
// key, the compose engine should try Fallback tokens in order. Lets an
// addition author specify "rough fit" — Portuguese ç on 'c', but fall
// back to comma if no 'c' exists at level 1.
func TestLetterOverlayFallback(t *testing.T) {
	phys := model.Physical{Name: "p", Keys: []string{"AC01", "AB08"}}
	// Hypothetical base with no 'c' at level 1, but has comma.
	odd := model.Transformation{Name: "odd", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"x", "X"}},
		"AB08": {Levels: []string{"comma", "less"}},
	}}
	portuguese := model.Addition{Name: "pt", LetterOverlays: map[string]model.LetterOverlay{
		"c": {
			Levels:   []string{"", "", "ccedilla", "Ccedilla"},
			Fallback: []string{"comma"},
		},
	}}
	r := ComposeFromParts(model.LayoutSpec{Name: "t"}, phys, odd, []model.Addition{portuguese})
	// Primary 'c' missing → fallback to 'comma' (AB08).
	got := r.Layout.Symbols["AB08"].Levels
	if len(got) < 3 || got[2] != "ccedilla" {
		t.Errorf("fallback should land ccedilla on AB08 (comma), got %v", got)
	}
	// AC01 (the 'x' key) untouched.
	if r.Layout.Symbols["AC01"].Levels[0] != "x" {
		t.Errorf("AC01 should still be 'x', got %v", r.Layout.Symbols["AC01"].Levels)
	}
}

// TestLetterOverlayFollowsLetterAcrossTransformations: a letter overlay
// defined for 'q' should land at whichever physical key 'q' occupies under
// the active transformation. This is the property that makes one `intl`
// addition work for QWERTY (q on AD01) and Dvorak (q on AB02) etc.
func TestLetterOverlayFollowsLetterAcrossTransformations(t *testing.T) {
	phys := model.Physical{Name: "p", Keys: []string{"AD01", "AB02"}}
	// QWERTY-style: q on AD01.
	qwerty := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AD01": {Levels: []string{"q", "Q"}},
		"AB02": {Levels: []string{"x", "X"}},
	}}
	// Dvorak-style: q on AB02.
	dvorak := model.Transformation{Name: "dv", Keys: map[string]model.KeySymbols{
		"AD01": {Levels: []string{"apostrophe", "quotedbl"}},
		"AB02": {Levels: []string{"q", "Q"}},
	}}
	intl := model.Addition{Name: "intl", LetterOverlays: map[string]model.LetterOverlay{
		"q": {Levels: []string{"", "", "adiaeresis", "Adiaeresis"}},
	}}

	r1 := ComposeFromParts(model.LayoutSpec{Name: "qwerty"}, phys, qwerty, []model.Addition{intl})
	if r1.Layout.Symbols["AD01"].Levels[2] != "adiaeresis" {
		t.Errorf("QWERTY: expected adiaeresis at AD01 level 3, got %v", r1.Layout.Symbols["AD01"].Levels)
	}
	if len(r1.Layout.Symbols["AB02"].Levels) > 2 && r1.Layout.Symbols["AB02"].Levels[2] != "" {
		t.Errorf("QWERTY: AB02 (x) should not gain umlaut, got %v", r1.Layout.Symbols["AB02"].Levels)
	}

	r2 := ComposeFromPartsWithSubs(model.LayoutSpec{Name: "dvorak"}, phys, dvorak, []model.Addition{intl}, nil)
	if r2.Layout.Symbols["AB02"].Levels[2] != "adiaeresis" {
		t.Errorf("Dvorak: expected adiaeresis at AB02 level 3, got %v", r2.Layout.Symbols["AB02"].Levels)
	}
	if r2.Layout.Symbols["AD01"].Levels[0] != "apostrophe" {
		t.Errorf("Dvorak: AD01 should keep apostrophe, got %v", r2.Layout.Symbols["AD01"].Levels)
	}
}

// TestSubstitutionTransformsAfterAdditions: substitutions run as a final
// pass and only affect single-token Latin letters. Multi-char symbols
// (dead_*, accent names) pass through unchanged.
func TestSubstitutionTransformsAfterAdditions(t *testing.T) {
	phys := model.Physical{Name: "p", Keys: []string{"AC01", "AC02", "TLDE"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A"}},
		"AC02": {Levels: []string{"s", "S"}},
		"TLDE": {Levels: []string{"grave", "asciitilde"}},
	}}
	// Addition adds a dead key — multi-token name.
	add := model.Addition{Name: "deadkey-tilde", Overlays: map[string]model.KeySymbols{
		"TLDE": {Levels: []string{"dead_grave", "dead_tilde"}},
	}}
	// Substitution replaces 'a'/'A'/'s'/'S' but leaves dead_grave alone.
	sub := model.Substitution{Name: "to-cyrillic", Map: map[string]string{
		"a": "Cyrillic_a", "A": "Cyrillic_A",
		"s": "Cyrillic_es", "S": "Cyrillic_ES",
	}}

	r := ComposeFromPartsWithSubs(
		model.LayoutSpec{Name: "test"},
		phys, trans,
		[]model.Addition{add},
		[]model.Substitution{sub},
	)
	if r.Layout.Symbols["AC01"].Levels[0] != "Cyrillic_a" {
		t.Errorf("AC01: %v", r.Layout.Symbols["AC01"].Levels)
	}
	if r.Layout.Symbols["TLDE"].Levels[0] != "dead_grave" {
		t.Errorf("TLDE dead_grave should pass through substitution: %v", r.Layout.Symbols["TLDE"].Levels)
	}
}

// TestSubstitutionChainHotfixPreempts: a narrow override placed before the
// broad substitution wins because each substitution sees the previous'
// output. By the time the broad substitution runs, the targeted symbol has
// already been replaced and no longer matches the broad map.
func TestSubstitutionChainHotfixPreempts(t *testing.T) {
	phys := model.Physical{Name: "p", Keys: []string{"AC01", "AD02"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A"}},
		"AD02": {Levels: []string{"w", "W"}},
	}}
	hotfix := model.Substitution{Name: "w-as-zhe", Map: map[string]string{
		"w": "Cyrillic_zhe", "W": "Cyrillic_ZHE",
	}}
	broad := model.Substitution{Name: "phonetic", Map: map[string]string{
		"a": "Cyrillic_a", "A": "Cyrillic_A",
		"w": "Cyrillic_ve", "W": "Cyrillic_VE",
	}}

	// Hotfix listed FIRST in chain wins.
	r := ComposeFromPartsWithSubs(model.LayoutSpec{Name: "t"}, phys, trans, nil,
		[]model.Substitution{hotfix, broad})
	if r.Layout.Symbols["AD02"].Levels[0] != "Cyrillic_zhe" {
		t.Errorf("hotfix should win for AD02, got %v", r.Layout.Symbols["AD02"].Levels)
	}
	if r.Layout.Symbols["AC01"].Levels[0] != "Cyrillic_a" {
		t.Errorf("broad should still apply to AC01, got %v", r.Layout.Symbols["AC01"].Levels)
	}
}

// TestNudgeModePreservesBaseDigitRow is the regression test for the
// 2026-05-12 azerty+intl bug. AZERTY's AE-row carries digits at L2
// (Shift-4 = 4, Shift-6 = 6, …). The intl addition's letter overlays
// for punctuation (apostrophe, minus, …) anchor by letter, so on
// AZERTY they land on AE04 (apostrophe key) and AE06 (minus key) —
// which on QWERTY are pure digit slots. In force mode the overlay
// destroyed L1/L2, taking the digits with it. With mode=nudge the
// existing base content survives and the overlay slides up to AltGr.
func TestNudgeModePreservesBaseDigitRow(t *testing.T) {
	phys := model.Physical{Name: "az", Keys: []string{"AE04", "AE06", "AE11"}}
	trans := model.Transformation{Name: "azerty", Keys: map[string]model.KeySymbols{
		// AZERTY-shaped: apostrophe at AE04 L1, "4" at L2.
		"AE04": {Levels: []string{"apostrophe", "4"}},
		"AE06": {Levels: []string{"minus", "6"}},
		// QWERTY-shaped control: minus at AE11 L1 (verifies the
		// "overlay L1 == base L1" no-op short-circuit).
		"AE11": {Levels: []string{"minus", "underscore"}},
	}}
	intl := model.Addition{Name: "intl", LetterOverlays: map[string]model.LetterOverlay{
		"apostrophe": {Levels: []string{"dead_acute", "dead_diaeresis", "apostrophe", "quotedbl"}, Mode: "nudge"},
		"minus":      {Levels: []string{"minus", "underscore", "yen", "dead_belowdot"}, Mode: "nudge"},
	}}
	r := ComposeFromParts(model.LayoutSpec{Name: "t"}, phys, trans, []model.Addition{intl})

	want := map[string][]string{
		// AE04 on AZERTY: apostrophe and "4" survive, dead_acute /
		// dead_diaeresis slide to L3/L4. The "minus" letter_overlay
		// also targets minus-containing keys; here it doesn't because
		// L1=apostrophe.
		"AE04": {"apostrophe", "4", "dead_acute", "dead_diaeresis"},
		// AE06: digit survives at L2. minus letter_overlay's L1=minus
		// is a same-value no-op; L2=underscore nudges past base "6"
		// to L3; L3=yen cascades to L4; L4=dead_belowdot has no slot
		// and drops. Digit preservation is the contract; the L4
		// luxury extra is sacrificed.
		"AE06": {"minus", "6", "underscore", "yen"},
		// AE11 on QWERTY-shape (no digit conflict): canonical
		// us-intl layout — same-value no-op for L1/L2, decorations
		// land cleanly at L3/L4.
		"AE11": {"minus", "underscore", "yen", "dead_belowdot"},
	}
	for k, w := range want {
		got := r.Layout.Symbols[k].Levels
		if len(got) != len(w) {
			t.Errorf("%s: got %v, want %v", k, got, w)
			continue
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s[%d]: got %q, want %q (full: %v)", k, i, got[i], w[i], got)
			}
		}
	}
}

// TestSubstitutionInverse: Substitution.Inverse swaps source/target.
func TestSubstitutionInverse(t *testing.T) {
	s := model.Substitution{Name: "fwd", Map: map[string]string{
		"a": "Cyrillic_a",
		"b": "Cyrillic_be",
	}}
	inv := s.Inverse()
	if inv.Map["Cyrillic_a"] != "a" {
		t.Errorf("inverse should map Cyrillic_a -> a, got %v", inv.Map)
	}
	if inv.Map["Cyrillic_be"] != "b" {
		t.Errorf("inverse should map Cyrillic_be -> b, got %v", inv.Map)
	}
}
