package inputmethod

import (
	"reflect"
	"testing"

	"github.com/lapingvino/flexkb/internal/model"
)

// TestFromComposeUnifiesTiers proves the design claim that a Compose
// chain is just a one-layer-up degenerate IM. We feed the post-prefix
// symbols of every sequence in the chain through the converted IM
// and assert the resulting commits match what an X-Compose consumer
// would do for the same keystrokes.
func TestFromComposeUnifiesTiers(t *testing.T) {
	chain := model.ComposeChain{
		Name:        "test-devanagari",
		Description: "subset of latin-devanagari-phonetic for engine test",
		Prefix:      "Multi_key",
		Sequences: []model.ComposeSequence{
			{ID: "krish", Input: []string{"x"}, Output: []string{"U0915", "U094D", "U0937"}},
			{ID: "krish-upper", Input: []string{"X"}, Output: []string{"U0915", "U094D", "U0937"}},
			{ID: "jnya", Input: []string{"j", "n"}, Output: []string{"U091C", "U094D", "U091E"}},
			{ID: "shri", Input: []string{"s", "h"}, Output: []string{"U0936", "U094D", "U0930", "U0940"}},
			{ID: "om", Input: []string{"o", "m"}, Output: []string{"U0950"}},
		},
	}
	im, err := FromCompose(chain)
	if err != nil {
		t.Fatalf("FromCompose: %v", err)
	}
	if im.Trigger.Mode != "prefix" || im.Trigger.PrefixKeysym != "Multi_key" {
		t.Errorf("trigger: got %+v, want {prefix, Multi_key}", im.Trigger)
	}

	// Each sequence's keystrokes (post-prefix) should produce its
	// output as a commit. Use a fresh session per sequence — in the
	// daemon, the post-commit session returns to "initial" anyway
	// (every leaf transition gotos initial), so reuse would also
	// work; per-sequence sessions just keep the test isolated.
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{"krish lowercase", []string{"x"}, "क्ष"},
		{"krish uppercase", []string{"X"}, "क्ष"},
		{"jnya two-key", []string{"j", "n"}, "ज्ञ"},
		{"shri two-key", []string{"s", "h"}, "श्री"},
		{"om two-key", []string{"o", "m"}, "ॐ"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess, err := NewSession(im)
			if err != nil {
				t.Fatal(err)
			}
			events := feedAll(t, sess, c.keys)
			commits := findCommits(events)
			if !reflect.DeepEqual(commits, []string{c.want}) {
				t.Errorf("got %v, want [%s]", commits, c.want)
			}
			if sess.State() != "initial" {
				t.Errorf("post-commit state %q, want initial", sess.State())
			}
		})
	}
}

// TestFromComposeUnknownSuffixDiscards — pressing the prefix key
// followed by a sequence that doesn't match any known chain entry
// must NOT emit anything to the application. X-Compose silently
// discards the partial sequence; the converted IM should match.
func TestFromComposeUnknownSuffixDiscards(t *testing.T) {
	chain := model.ComposeChain{
		Name:   "test",
		Prefix: "Multi_key",
		Sequences: []model.ComposeSequence{
			{Input: []string{"j", "n"}, Output: []string{"U091C"}},
		},
	}
	im, err := FromCompose(chain)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := NewSession(im)
	// j puts us in the j-prefix state.
	feedAll(t, sess, []string{"j"})
	if sess.State() == "initial" {
		t.Errorf("after j, still in initial — trie didn't advance")
	}
	// q at the j-prefix is unmatched; wildcard fallback returns to
	// initial without emitting anything.
	events := sess.Feed("q")
	if len(findCommits(events)) > 0 {
		t.Errorf("unknown suffix emitted commits: %v", findCommits(events))
	}
	if sess.State() != "initial" {
		t.Errorf("post-discard state %q, want initial", sess.State())
	}
}

// TestFromComposeRejectsEmptyChain — an empty chain is a data bug,
// not a "do nothing" config; the converter must surface it.
func TestFromComposeRejectsEmptyChain(t *testing.T) {
	if _, err := FromCompose(model.ComposeChain{Name: "empty"}); err == nil {
		t.Error("expected error for empty chain")
	}
}

// TestFromComposeRejectsBadCodepoint — invalid U-escapes in the
// output list must fail conversion rather than silently emit garbage.
func TestFromComposeRejectsBadCodepoint(t *testing.T) {
	chain := model.ComposeChain{
		Name: "bad",
		Sequences: []model.ComposeSequence{
			{Input: []string{"x"}, Output: []string{"not-a-u-escape"}},
		},
	}
	if _, err := FromCompose(chain); err == nil {
		t.Error("expected error for bad codepoint")
	}
}
