package imsession

import (
	"reflect"
	"testing"

	"github.com/lapingvino/flexkb/internal/inputmethod"
	"github.com/lapingvino/flexkb/internal/model"
	"github.com/lapingvino/flexkb/internal/runtime"
)

// testResolver builds a Resolver against a minimal layout that
// maps three known scancodes — AC01 (a), AC02 (s), space — so
// the tests can drive HandleKey with realistic input.
func testResolver() *runtime.Resolver {
	return runtime.NewResolver(model.ComposedLayout{
		Symbols: map[string]model.KeySymbols{
			"AC01": {Levels: []string{"a", "A"}},
			"AC02": {Levels: []string{"s", "S"}},
			"SPCE": {Levels: []string{"space"}},
		},
	})
}

// scancodeFor returns the Linux evdev code for a known xkb
// keyname. We only need a few in the tests; using the runtime's
// reverse table keeps the test honest about which scancodes
// trigger which symbols.
func scancodeFor(t *testing.T, name string) uint32 {
	t.Helper()
	code, ok := runtime.LinuxScancodeForXKBName(name)
	if !ok {
		t.Fatalf("no scancode for %q", name)
	}
	return code
}

// TestPassThroughWhenNoIM — without an IM loaded, the session
// emits the resolved symbol as a single-commit batch.
func TestPassThroughWhenNoIM(t *testing.T) {
	s, err := New(testResolver(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := s.HandleKey(scancodeFor(t, "AC01"), 1)
	if !hasAction(got, CommitText{Text: "a"}) {
		t.Errorf("missing commit for 'a': %v", got)
	}
	if !hasFinishCommit(got) {
		t.Errorf("missing FinishCommit: %v", got)
	}
}

// TestKeyReleaseIgnored — release events (state=0) produce no
// actions. The IM tier is press-driven.
func TestKeyReleaseIgnored(t *testing.T) {
	s, _ := New(testResolver(), nil)
	got := s.HandleKey(scancodeFor(t, "AC01"), 0)
	if len(got) != 0 {
		t.Errorf("release produced actions: %v", got)
	}
}

// TestShiftModifierSelectsL2 — handle_modifiers with Shift bit
// set drives the resolver to L2.
func TestShiftModifierSelectsL2(t *testing.T) {
	s, _ := New(testResolver(), nil)
	s.HandleModifiers(1, 0, 0) // depressed=Shift
	got := s.HandleKey(scancodeFor(t, "AC01"), 1)
	if !hasAction(got, CommitText{Text: "A"}) {
		t.Errorf("missing commit for 'A': %v", got)
	}
}

// TestAltGrModifierSelectsL3 — bit 7 = ISO_Level3_Shift (AltGr).
func TestAltGrModifierSelectsL3(t *testing.T) {
	r := runtime.NewResolver(model.ComposedLayout{
		Symbols: map[string]model.KeySymbols{
			"AC01": {Levels: []string{"a", "A", "aacute", "Aacute"}},
		},
	})
	s, _ := New(r, nil)
	s.HandleModifiers(1<<7, 0, 0)
	got := s.HandleKey(scancodeFor(t, "AC01"), 1)
	if !hasAction(got, CommitText{Text: "aacute"}) {
		t.Errorf("missing commit for 'aacute': %v", got)
	}
}

// TestUnmappedScancodePassesThrough — function keys etc. yield a
// PassThrough action so the transport can forward to the app.
func TestUnmappedScancodePassesThrough(t *testing.T) {
	s, _ := New(testResolver(), nil)
	got := s.HandleKey(99999, 1)
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1: %v", len(got), got)
	}
	if _, ok := got[0].(PassThrough); !ok {
		t.Errorf("got %#v, want PassThrough", got[0])
	}
}

// TestIMSwallowsSymbolNoCommitEmitted — a multi-keystroke IM
// state where the first key buffers without emitting must
// produce zero actions (so the transport doesn't issue a
// spurious FinishCommit).
func TestIMSwallowsSymbolNoCommitEmitted(t *testing.T) {
	im, err := inputmethod.Parse([]byte(`
name: test
trigger: { mode: always }
states:
  - id: initial
    transitions:
      - match: "a"
        append_buffer: true
        goto: buffering
  - id: buffering
    transitions:
      - match: "a"
        commit: "aa"
        clear_buffer: true
        goto: initial
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(testResolver(), im)
	if err != nil {
		t.Fatal(err)
	}
	// First 'a' — IM buffers it, no events emitted.
	got1 := s.HandleKey(scancodeFor(t, "AC01"), 1)
	if len(got1) != 0 {
		t.Errorf("first 'a' produced actions: %v", got1)
	}
	// Second 'a' — IM commits "aa".
	got2 := s.HandleKey(scancodeFor(t, "AC01"), 1)
	if !hasAction(got2, CommitText{Text: "aa"}) {
		t.Errorf("missing commit for 'aa': %v", got2)
	}
	if !hasFinishCommit(got2) {
		t.Errorf("missing FinishCommit on commit: %v", got2)
	}
}

// TestFinishCommitSerialMonotonic — each emitted batch
// increments the serial; transports rely on monotonic serials
// to ack the right batch.
func TestFinishCommitSerialMonotonic(t *testing.T) {
	s, _ := New(testResolver(), nil)
	got1 := s.HandleKey(scancodeFor(t, "AC01"), 1)
	got2 := s.HandleKey(scancodeFor(t, "AC02"), 1)
	s1 := findSerial(got1)
	s2 := findSerial(got2)
	if !(s2 > s1 && s1 > 0) {
		t.Errorf("serials not monotonic: got1=%d got2=%d", s1, s2)
	}
}

// TestKeymapPendingSuppressesCommits — between MarkKeymapPending
// and MarkKeymapApplied the daemon shouldn't emit anything (the
// resolver might be stale).
func TestKeymapPendingSuppressesCommits(t *testing.T) {
	im, err := inputmethod.Parse([]byte(`
name: t
trigger: { mode: always }
states:
  - id: initial
    transitions:
      - match: "a"
        commit: "A"
`))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(testResolver(), im)
	s.MarkKeymapPending()
	got := s.HandleKey(scancodeFor(t, "AC01"), 1)
	if len(got) != 0 {
		t.Errorf("keymap-pending didn't suppress: %v", got)
	}
	s.MarkKeymapApplied()
	got = s.HandleKey(scancodeFor(t, "AC01"), 1)
	if !hasAction(got, CommitText{Text: "A"}) {
		t.Errorf("after MarkKeymapApplied, expected commit: %v", got)
	}
}

// --- helpers ---

func hasAction(actions []Action, want Action) bool {
	for _, a := range actions {
		if reflect.DeepEqual(a, want) {
			return true
		}
	}
	return false
}

func hasFinishCommit(actions []Action) bool {
	for _, a := range actions {
		if _, ok := a.(FinishCommit); ok {
			return true
		}
	}
	return false
}

func findSerial(actions []Action) uint32 {
	for _, a := range actions {
		if fc, ok := a.(FinishCommit); ok {
			return fc.Serial
		}
	}
	return 0
}
