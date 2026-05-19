package mimimport

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lapingvino/flexkb/internal/inputmethod"
)

// TestParseSimpleSexp — sanity-check the parser on a hand-rolled
// mim-shaped snippet. Covers the four node kinds (list, symbol,
// string, char) and one nested list.
func TestParseSimpleSexp(t *testing.T) {
	src := `
;; comment ignored
(input-method da post)
(description "Danish postfix")
(map (trans ("ae" ?æ) ("AE" ?Æ)))
`
	nodes, err := ParseSexp(src)
	if err != nil {
		t.Fatalf("ParseSexp: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d top-level nodes, want 3", len(nodes))
	}
	// Verify the (input-method da post) form structurally.
	im := nodes[0]
	if im.Kind != "list" || len(im.Children) != 3 ||
		im.Children[0].Sym != "input-method" || im.Children[1].Sym != "da" || im.Children[2].Sym != "post" {
		t.Errorf("input-method form: %+v", im)
	}
	// The (map ...) form contains a (trans ...) which contains rules.
	mapForm := nodes[2]
	if mapForm.Kind != "list" || mapForm.Children[0].Sym != "map" {
		t.Errorf("map form head wrong: %+v", mapForm)
	}
}

// TestParseCharLiterals — the char-literal escape forms we
// actually use in real mim files.
func TestParseCharLiterals(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"?a", "a"},
		{"?Å", "Å"},
		{`?\n`, "\n"},
		{`?æ`, "æ"},
		{`?\U00010437`, "𐐷"},
		{`?\x41`, "A"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			nodes, err := ParseSexp(c.in)
			if err != nil {
				t.Fatalf("ParseSexp(%q): %v", c.in, err)
			}
			if len(nodes) != 1 || nodes[0].Kind != "char" {
				t.Fatalf("expected one char node, got %+v", nodes)
			}
			if nodes[0].Str != c.want {
				t.Errorf("got %q, want %q", nodes[0].Str, c.want)
			}
		})
	}
}

// TestConvertSyntheticPostfix — mim-shaped IM converted in
// isolation (no filesystem dependency). The "ae → æ" pattern is
// the same shape as half of m17n-db's *-post.mim files.
func TestConvertSyntheticPostfix(t *testing.T) {
	src := `
(input-method da post)
(description "Danish postfix test")
(title "da-post")
(map
 (trans
  ("ae" ?æ)
  ("AE" ?Æ)
  ("oe" ?ø)))
(state
  (init
    (trans)))
`
	im, warnings, err := Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(warnings) > 0 {
		t.Logf("warnings: %v", warnings)
	}
	if im.Name != "da-post" {
		t.Errorf("name: got %q, want da-post", im.Name)
	}
	// Drive the FSM with each rule's input keystrokes and verify
	// the commit matches the rule's output.
	cases := []struct {
		input []string
		want  string
	}{
		{[]string{"a", "e"}, "æ"},
		{[]string{"A", "E"}, "Æ"},
		{[]string{"o", "e"}, "ø"},
	}
	for _, c := range cases {
		t.Run(string(c.want), func(t *testing.T) {
			sess, err := inputmethod.NewSession(im)
			if err != nil {
				t.Fatal(err)
			}
			var commits []string
			for _, k := range c.input {
				for _, ev := range sess.Feed(k) {
					if ce, ok := ev.(inputmethod.CommitEvent); ok {
						commits = append(commits, ce.Text)
					}
				}
			}
			if !reflect.DeepEqual(commits, []string{c.want}) {
				t.Errorf("got %v, want [%s]", commits, c.want)
			}
		})
	}
}

// TestConvertRealDaPost — exercises the parser + converter
// against m17n-db's actual da-post.mim file. Gated on the file
// being present so CI without m17n-db can still pass.
func TestConvertRealDaPost(t *testing.T) {
	const path = "/usr/share/m17n/da-post.mim"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("m17n-db not installed (%s): %v", path, err)
	}
	im, warnings, err := Convert(string(src))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	t.Logf("converted name=%q states=%d warnings=%d", im.Name, len(im.States), len(warnings))
	if im.Name != "da-post" {
		t.Errorf("name: got %q, want da-post", im.Name)
	}
	// Spot-check the well-known da-post bindings still produce
	// what they should after round-tripping through the FSM.
	cases := []struct {
		input []string
		want  string
	}{
		{[]string{"a", "a"}, "å"},
		{[]string{"A", "A"}, "Å"},
		{[]string{"a", "e"}, "æ"},
		{[]string{"o", "e"}, "ø"},
		{[]string{"e", "'"}, "é"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			sess, err := inputmethod.NewSession(im)
			if err != nil {
				t.Fatal(err)
			}
			var commits []string
			for _, k := range c.input {
				for _, ev := range sess.Feed(k) {
					if ce, ok := ev.(inputmethod.CommitEvent); ok {
						commits = append(commits, ce.Text)
					}
				}
			}
			if len(commits) == 0 || commits[0] != c.want {
				t.Errorf("got %v, want [%s]", commits, c.want)
			}
		})
	}
}

// TestConvertWarnsOnUnsupportedForms — rules using symbols (key
// sequences) or list-form output (action expressions) get warned
// and skipped, not silently dropped.
func TestConvertWarnsOnUnsupportedForms(t *testing.T) {
	src := `
(input-method test t)
(map
 (trans
  ("ae" ?æ)
  ((Tab) ?\t)
  ("xy" (insert "z"))))
(state (init (trans)))
`
	_, warnings, err := Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// Expect at least two warnings: one for the keysym input,
	// one for the action-form output.
	if len(warnings) < 2 {
		t.Errorf("got %d warnings, want >=2: %v", len(warnings), warnings)
	}
}

// TestSurveyM17nCoverage walks every .mim file in m17n-db and
// reports the conversion-coverage statistics. Skipped by default
// (gated on FLEXKB_MIM_SURVEY=1) since it's a one-off coverage
// audit, not a regression test. Useful for tracking how the v1
// importer's structured subset fares against real upstream data.
func TestSurveyM17nCoverage(t *testing.T) {
	if os.Getenv("FLEXKB_MIM_SURVEY") != "1" {
		t.Skip("set FLEXKB_MIM_SURVEY=1 to run the m17n-db coverage audit")
	}
	dir := "/usr/share/m17n"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("m17n-db not installed (%s): %v", dir, err)
	}
	var clean, withWarn, failed int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".mim") {
			continue
		}
		src, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			failed++
			continue
		}
		_, warns, err := Convert(string(src))
		switch {
		case err != nil:
			failed++
		case len(warns) == 0:
			clean++
		default:
			withWarn++
		}
	}
	t.Logf("clean=%d  with-warnings=%d  failed=%d  total=%d",
		clean, withWarn, failed, clean+withWarn+failed)
}
