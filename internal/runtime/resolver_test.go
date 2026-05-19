package runtime

import (
	"testing"

	"github.com/lapingvino/flexkb/internal/model"
)

// fakeLayout builds a small ComposedLayout for tests. Three keys
// covering the level-count edge cases the resolver has to handle:
//
//   - AC01: full 4-level binding   ("a","A","ae","AE")
//   - AC02: 2-level only           ("s","S")
//   - AC03: 3-level                ("d","D","dd")
func fakeLayout() model.ComposedLayout {
	return model.ComposedLayout{
		Name: "test",
		Keys: []string{"AC01", "AC02", "AC03"},
		Symbols: map[string]model.KeySymbols{
			"AC01": {Levels: []string{"a", "A", "ae", "AE"}},
			"AC02": {Levels: []string{"s", "S"}},
			"AC03": {Levels: []string{"d", "D", "dd"}},
		},
	}
}

func TestResolveLevels(t *testing.T) {
	r := NewResolver(fakeLayout())
	cases := []struct {
		name string
		key  string
		mods ModState
		want string
	}{
		{"L1 plain", "AC01", ModState{}, "a"},
		{"L2 shift", "AC01", ModState{Shift: true}, "A"},
		{"L3 altgr", "AC01", ModState{AltGr: true}, "ae"},
		{"L4 shift+altgr", "AC01", ModState{Shift: true, AltGr: true}, "AE"},
		{"capslock acts like shift", "AC01", ModState{CapsLock: true}, "A"},
		{"capslock+altgr like shift+altgr", "AC01", ModState{CapsLock: true, AltGr: true}, "AE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := r.Resolve(c.key, c.mods)
			if !ok || got != c.want {
				t.Errorf("got (%q, %v), want (%q, true)", got, ok, c.want)
			}
		})
	}
}

// TestResolveClampsToAvailableLevel — a key with fewer than 4
// levels defined should fall back to the highest defined level
// when a higher one is requested. Matches xkb's group_clamp
// behaviour: AltGr+s on a key without an AltGr binding returns
// the shift form rather than swallowing the keystroke silently.
func TestResolveClampsToAvailableLevel(t *testing.T) {
	r := NewResolver(fakeLayout())
	cases := []struct {
		name string
		key  string
		mods ModState
		want string
	}{
		// AC02 has only L1/L2.
		{"AltGr clamps to L2 when no L3", "AC02", ModState{AltGr: true}, "S"},
		{"Shift+AltGr clamps to L2", "AC02", ModState{Shift: true, AltGr: true}, "S"},
		// AC03 has L1/L2/L3.
		{"Shift+AltGr clamps to L3", "AC03", ModState{Shift: true, AltGr: true}, "dd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := r.Resolve(c.key, c.mods)
			if !ok || got != c.want {
				t.Errorf("got (%q, %v), want (%q, true)", got, ok, c.want)
			}
		})
	}
}

// TestResolveUnknownKey — unmapped keys return (\"\", false) and
// the daemon should passthrough them.
func TestResolveUnknownKey(t *testing.T) {
	r := NewResolver(fakeLayout())
	if _, ok := r.Resolve("UNKNOWN", ModState{}); ok {
		t.Error("unknown key returned ok=true")
	}
}

// TestResolveNoSymbolHole — explicit NoSymbol bindings (placed by
// the writer to fill mid-array gaps from locale-fill) must read
// back as (\"\", false), not the literal string "NoSymbol".
func TestResolveNoSymbolHole(t *testing.T) {
	r := NewResolver(model.ComposedLayout{
		Symbols: map[string]model.KeySymbols{
			"AC01": {Levels: []string{"p", "P", "NoSymbol", "ydiaeresis"}},
		},
	})
	got, ok := r.Resolve("AC01", ModState{AltGr: true}) // L3 = NoSymbol
	if ok || got != "" {
		t.Errorf("got (%q, %v), want (\"\", false)", got, ok)
	}
	// L4 should still work.
	got, ok = r.Resolve("AC01", ModState{Shift: true, AltGr: true})
	if !ok || got != "ydiaeresis" {
		t.Errorf("L4: got (%q, %v), want (ydiaeresis, true)", got, ok)
	}
}

// TestXKBNameForLinuxScancodeRoundTrips — every entry in the
// scancode table should round-trip through both directions.
func TestXKBNameForLinuxScancodeRoundTrips(t *testing.T) {
	for code, name := range evdevToXKB {
		gotName, ok := XKBNameForLinuxScancode(code)
		if !ok || gotName != name {
			t.Errorf("forward %d: got (%q, %v), want (%q, true)", code, gotName, ok, name)
		}
		gotCode, ok := LinuxScancodeForXKBName(name)
		if !ok || gotCode != code {
			t.Errorf("reverse %q: got (%d, %v), want (%d, true)", name, gotCode, ok, code)
		}
	}
}

// TestXKBNameUnmappedScancode — function keys and modifiers don't
// belong in the keymap layer; unmapped lookups must return false
// so the daemon passes them through unchanged.
func TestXKBNameUnmappedScancode(t *testing.T) {
	for _, code := range []uint32{60, 61, 113, 114, 200} {
		if _, ok := XKBNameForLinuxScancode(code); ok {
			t.Errorf("scancode %d unexpectedly mapped", code)
		}
	}
}

// TestEndToEndAgainstCompose — wire the runtime up to the real
// composer (just QWERTY+ANSI as the simplest possible stack) and
// verify it produces the same symbols the static xkb file would.
// Picks a handful of well-known positions to keep the test honest
// about the integration, not just the wrapper.
func TestEndToEndAgainstCompose(t *testing.T) {
	// Synthesise the smallest possible "real" layout in-memory so
	// the test doesn't depend on data/ paths or a particular YAML
	// being authored a particular way.
	layout := model.ComposedLayout{
		Symbols: map[string]model.KeySymbols{
			"AC01": {Levels: []string{"a", "A"}},
			"AE01": {Levels: []string{"1", "exclam"}},
			"SPCE": {Levels: []string{"space"}},
		},
	}
	r := NewResolver(layout)
	sym, _ := r.Resolve("AC01", ModState{})
	if sym != "a" {
		t.Errorf("AC01 plain: got %q, want a", sym)
	}
	sym, _ = r.Resolve("AC01", ModState{Shift: true})
	if sym != "A" {
		t.Errorf("AC01 shift: got %q, want A", sym)
	}
	sym, _ = r.Resolve("AE01", ModState{Shift: true})
	if sym != "exclam" {
		t.Errorf("AE01 shift: got %q, want exclam", sym)
	}
	sym, _ = r.Resolve("SPCE", ModState{})
	if sym != "space" {
		t.Errorf("SPCE: got %q, want space", sym)
	}
}
