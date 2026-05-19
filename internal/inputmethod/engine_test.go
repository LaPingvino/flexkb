package inputmethod

import (
	"reflect"
	"testing"
)

// feedAll runs each symbol in seq through the session and concatenates
// the produced events. Convenient for asserting end-to-end against a
// keystroke sequence.
func feedAll(t *testing.T, s *Session, seq []string) []Event {
	t.Helper()
	var all []Event
	for _, sym := range seq {
		all = append(all, s.Feed(sym)...)
	}
	return all
}

// findCommits filters an event stream down to the user-visible commits.
// Most assertions are easier expressed as "the IM committed X then Y"
// than as the full event sequence including preedit churn.
func findCommits(events []Event) []string {
	var out []string
	for _, e := range events {
		if c, ok := e.(CommitEvent); ok {
			out = append(out, c.Text)
		}
	}
	return out
}

// findPreedits returns the preedit-text history. Useful for verifying
// in-flight composition state without coupling to state-change events.
func findPreedits(events []Event) []string {
	var out []string
	for _, e := range events {
		if p, ok := e.(PreeditEvent); ok {
			out = append(out, p.Text)
		}
	}
	return out
}

// TestOneStateComposeShape exercises the simplest IM shape: a single
// state with explicit-match transitions that commit a literal output
// — the same role X11 Compose chains play.
func TestOneStateComposeShape(t *testing.T) {
	yamlSrc := []byte(`
name: test-compose
trigger: { mode: prefix, prefix_keysym: Multi_key }
states:
  - id: initial
    transitions:
      - match: x
        commit: "क्ष"
      - match: o
        commit: "ॐ"
`)
	im, err := Parse(yamlSrc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sess, err := NewSession(im)
	if err != nil {
		t.Fatal(err)
	}
	got := findCommits(sess.Feed("x"))
	if !reflect.DeepEqual(got, []string{"क्ष"}) {
		t.Errorf("x: got %v, want [क्ष]", got)
	}
	got = findCommits(sess.Feed("o"))
	if !reflect.DeepEqual(got, []string{"ॐ"}) {
		t.Errorf("o: got %v, want [ॐ]", got)
	}
}

// TestPassthroughOnNoMatch — an unmatched symbol falls through as a
// PassthroughEvent and the session state is unchanged.
func TestPassthroughOnNoMatch(t *testing.T) {
	im, err := Parse([]byte(`
name: test
trigger: { mode: prefix }
states:
  - id: initial
    transitions:
      - match: x
        commit: hit
`))
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := NewSession(im)
	events := sess.Feed("z")
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	pt, ok := events[0].(PassthroughEvent)
	if !ok || pt.Symbol != "z" {
		t.Errorf("got %#v, want PassthroughEvent{z}", events[0])
	}
	if sess.State() != "initial" {
		t.Errorf("state drifted to %q on no-match", sess.State())
	}
}

// TestMultiStateDecomposition — a two-state IM that buffers letters
// and commits the buffer concatenation on space. Models the
// "stateful syllable assembly" archetype (Hangul, Indic conjuncts,
// Vietnamese tone composition).
func TestMultiStateDecomposition(t *testing.T) {
	im, err := Parse([]byte(`
name: test-buffer
trigger: { mode: always }
states:
  - id: initial
    transitions:
      - match_class: latin-letter
        append_buffer: true
        preedit: "${buffer}"
        goto: buffering
  - id: buffering
    transitions:
      - match_class: latin-letter
        append_buffer: true
        preedit: "${buffer}"
      - match: space
        commit: "${buffer}"
        clear_buffer: true
        goto: initial
      - match: BackSpace
        pop_buffer: true
        preedit: "${buffer}"
        goto_if_empty: initial
`))
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := NewSession(im)

	// "h", "e", "l", "l", "o" — buffer grows, preedit reflects it.
	events := feedAll(t, sess, []string{"h", "e", "l", "l", "o"})
	preedits := findPreedits(events)
	want := []string{"h", "he", "hel", "hell", "hello"}
	if !reflect.DeepEqual(preedits, want) {
		t.Errorf("preedit trail: got %v, want %v", preedits, want)
	}
	if findCommits(events) != nil {
		t.Errorf("commits before space: got %v, want none", findCommits(events))
	}
	// Space commits.
	commits := findCommits(sess.Feed("space"))
	if !reflect.DeepEqual(commits, []string{"hello"}) {
		t.Errorf("space commit: got %v, want [hello]", commits)
	}
	if sess.State() != "initial" {
		t.Errorf("post-commit state: got %q, want initial", sess.State())
	}
	if len(sess.Buffer()) != 0 {
		t.Errorf("post-commit buffer non-empty: %v", sess.Buffer())
	}

	// BackSpace from empty initial — passthrough (no buffer to pop,
	// no transition matches in 'initial' for BackSpace).
	bs := sess.Feed("BackSpace")
	if _, ok := bs[0].(PassthroughEvent); !ok {
		t.Errorf("empty-initial BackSpace: got %#v, want passthrough", bs[0])
	}

	// "a", "b", BackSpace, BackSpace — buffer fills then empties,
	// the second BackSpace lands us back in initial via goto_if_empty.
	feedAll(t, sess, []string{"a", "b"})
	if sess.State() != "buffering" {
		t.Errorf("after a,b state %q, want buffering", sess.State())
	}
	feedAll(t, sess, []string{"BackSpace", "BackSpace"})
	if sess.State() != "initial" {
		t.Errorf("after two BackSpaces state %q, want initial", sess.State())
	}
}

// TestLookupTemplateExpansion — exercises ${lookup.top} via the stub
// runLookup which returns the buffer as a one-element candidate
// list. Real dictionary lookup will use the same expansion mechanism.
func TestLookupTemplateExpansion(t *testing.T) {
	im, err := Parse([]byte(`
name: test-lookup
trigger: { mode: always }
states:
  - id: initial
    transitions:
      - match: space
        lookup: { dictionary: any }
        commit: "${lookup.top}"
        clear_buffer: true
      - match_class: latin-letter
        append_buffer: true
`))
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := NewSession(im)
	feedAll(t, sess, []string{"n", "i"})
	commits := findCommits(sess.Feed("space"))
	if !reflect.DeepEqual(commits, []string{"ni"}) {
		t.Errorf("got %v, want [ni]", commits)
	}
}

// TestWildcardMatch — Match: "*" catches anything that hasn't matched
// an earlier transition. Useful for "drain the buffer if user types
// something unexpected".
func TestWildcardMatch(t *testing.T) {
	im, err := Parse([]byte(`
name: test-wildcard
trigger: { mode: always }
states:
  - id: initial
    transitions:
      - match: x
        commit: SPECIAL
      - match: "*"
        commit: "${match}"
`))
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := NewSession(im)
	commits := findCommits(sess.Feed("x"))
	if !reflect.DeepEqual(commits, []string{"SPECIAL"}) {
		t.Errorf("x: got %v, want [SPECIAL]", commits)
	}
	commits = findCommits(sess.Feed("y"))
	if !reflect.DeepEqual(commits, []string{"y"}) {
		t.Errorf("y wildcard: got %v, want [y]", commits)
	}
}

// TestCustomMatchClassesOverrideBuiltins — an IM file can redefine a
// builtin class name to tighten or replace its membership.
func TestCustomMatchClassesOverrideBuiltins(t *testing.T) {
	im, err := Parse([]byte(`
name: test-override
trigger: { mode: always }
match_classes:
  latin-letter: [a, b, c]
states:
  - id: initial
    transitions:
      - match_class: latin-letter
        commit: hit
`))
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := NewSession(im)
	if got := findCommits(sess.Feed("a")); !reflect.DeepEqual(got, []string{"hit"}) {
		t.Errorf("a (in override): got %v, want [hit]", got)
	}
	// 'z' is in the builtin latin-letter set but NOT in the override.
	events := sess.Feed("z")
	if _, ok := events[0].(PassthroughEvent); !ok {
		t.Errorf("z (excluded by override): got %#v, want passthrough", events[0])
	}
}

// TestEffectiveBackspaceDefaults — verifies the auto-default rules
// from docs/DESIGN-IME.md "Per-IM defaults": dictionary-using IMs
// default to reopen, multi-state IMs to decompose, single-state IMs
// to passthrough, explicit value always wins.
func TestEffectiveBackspaceDefaults(t *testing.T) {
	cases := []struct {
		name string
		im   InputMethod
		want string
	}{
		{
			name: "explicit wins",
			im:   InputMethod{Backspace: "passthrough", Dictionaries: []DictionaryRef{{ID: "d"}}, States: []State{{ID: "a"}, {ID: "b"}}},
			want: "passthrough",
		},
		{
			name: "has dictionary → reopen",
			im:   InputMethod{Dictionaries: []DictionaryRef{{ID: "d"}}, States: []State{{ID: "a"}}},
			want: "reopen",
		},
		{
			name: "multi-state no dict → decompose",
			im:   InputMethod{States: []State{{ID: "a"}, {ID: "b"}}},
			want: "decompose",
		},
		{
			name: "single state, no dict → passthrough",
			im:   InputMethod{States: []State{{ID: "a"}}},
			want: "passthrough",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.im.EffectiveBackspace(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestEffectiveLearningDefaults — dictionary presence flips the
// default; explicit value overrides.
func TestEffectiveLearningDefaults(t *testing.T) {
	cases := []struct {
		name string
		im   InputMethod
		want string
	}{
		{"explicit disabled", InputMethod{Learning: "disabled", Dictionaries: []DictionaryRef{{ID: "d"}}}, "disabled"},
		{"has dictionary", InputMethod{Dictionaries: []DictionaryRef{{ID: "d"}}}, "enabled"},
		{"no dictionary", InputMethod{}, "disabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.im.EffectiveLearning(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestLoaderRejectsBadFiles — the parser must reject missing name,
// missing states, and dangling goto references.
func TestLoaderRejectsBadFiles(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"no name", `states: [{id: a}]`},
		{"no states", `name: x`},
		{"duplicate state id", `name: x
trigger: { mode: always }
states:
  - id: a
  - id: a`},
		{"dangling goto", `name: x
trigger: { mode: always }
states:
  - id: a
    transitions:
      - match: q
        goto: nonexistent`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.src)); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}
