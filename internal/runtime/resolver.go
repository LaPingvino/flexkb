// Package runtime is the in-memory evaluator the IME daemon uses
// per-keystroke. Today the same composition rules live in
// internal/compose at build time, producing static xkb files; this
// package exposes the same result graph as a runtime API so the
// daemon can resolve (keycode, modifiers) → symbol without going
// through libxkbcommon or a compiled xkb tree.
//
// The runtime evaluator deliberately does no composition of its own.
// It consumes a pre-resolved model.ComposedLayout (built by
// internal/compose, which already walked Physical × Transformation ×
// Additions × Substitutions). The runtime's only job is the
// modifier→level mapping and the keycode→keyname lookup.
package runtime

import "github.com/lapingvino/flexkb/internal/model"

// Resolver wraps a ComposedLayout and answers per-keystroke queries.
// Safe for concurrent reads; the wrapped ComposedLayout is treated
// as immutable.
type Resolver struct {
	layout model.ComposedLayout
}

// NewResolver wraps a layout for runtime keystroke resolution. The
// layout must come from internal/compose (or have the same shape:
// Symbols keyed by xkb keycode names, each KeySymbols.Levels in
// xkb's L1..L4 order).
func NewResolver(layout model.ComposedLayout) *Resolver {
	return &Resolver{layout: layout}
}

// ModState carries the modifier flags relevant to level selection.
// Other modifiers (Ctrl, Alt, Super) don't influence the level the
// keymap returns — they're application-level shortcuts — so they
// aren't tracked here. The daemon may carry them separately for
// other purposes.
type ModState struct {
	// Shift selects L2 over L1 (and L4 over L3 when AltGr is also
	// held). The standard "uppercase" modifier on Latin keyboards.
	Shift bool
	// AltGr (xkb's level3) selects L3 over L1 (and L4 over L2 when
	// Shift is also held). The "international symbols" modifier on
	// most layouts.
	AltGr bool
	// CapsLock toggles letter case for keys that participate in
	// case-aware level promotion. Most non-letter keys ignore it.
	// The resolver tracks it but level selection uses it the same
	// way Shift does — that matches xkb's default ALPHABETIC type
	// behaviour and is fine for the dominant case; the rare
	// per-key type variations stay in the static xkb path.
	CapsLock bool
}

// Resolve returns the symbol bound to keyName at the level selected
// by mods. Returns ("", false) when no binding exists for the key,
// or when the selected level is past the end of the binding's
// Levels list (e.g. asking for L4 of a key that only defines L1/L2).
//
// keyName is the xkb keycode mnemonic without angle brackets
// ("AC01", "AE01", "SPCE"). The daemon obtains it via
// XKBNameForLinuxScancode below.
func (r *Resolver) Resolve(keyName string, mods ModState) (string, bool) {
	sym, ok := r.layout.Symbols[keyName]
	if !ok {
		return "", false
	}
	level := selectLevel(mods, len(sym.Levels))
	if level < 0 || level >= len(sym.Levels) {
		return "", false
	}
	v := sym.Levels[level]
	if v == "" || v == "NoSymbol" {
		return "", false
	}
	return v, true
}

// selectLevel applies the xkb FOUR_LEVEL_ALPHABETIC mapping:
//
//	     | no shift | shift |
//	-----+----------+-------+
//	-AltGr|   L1    |  L2   |
//	+AltGr|   L3    |  L4   |
//
// CapsLock is treated as Shift for letter keys. The caller is
// responsible for honouring per-key types in the rare case the
// layout's static composition produced a non-FOUR_LEVEL_ALPHABETIC
// type binding; for the dominant Latin/intl shape this is correct.
//
// available is the binding's actual Levels length so we can clamp
// the request: a key with only L1/L2 defined and a Shift+AltGr
// query falls back to L2 (closest defined upper level) rather than
// returning empty — matches xkb's group_clamp behaviour and avoids
// "AltGr+letter on a key without an AltGr binding silently swallows
// the keystroke."
func selectLevel(mods ModState, available int) int {
	shifted := mods.Shift || mods.CapsLock
	level := 0
	if shifted && mods.AltGr {
		level = 3
	} else if mods.AltGr {
		level = 2
	} else if shifted {
		level = 1
	}
	// Clamp to the highest defined level <= requested.
	for level >= available && level > 0 {
		level--
	}
	return level
}

// XKBNameForLinuxScancode translates a Linux evdev scancode (the
// raw integer in struct input_event.code) to the xkb keycode
// mnemonic used inside ComposedLayout.Symbols. Returns ("", false)
// for unmapped scancodes — the daemon should passthrough those
// (function keys, media keys, modifier keys themselves, etc.).
//
// Wayland clients usually receive Linux evdev scancodes via the
// wl_keyboard.key event. The X11 convention is +8 (so X11 keycode
// 38 = Linux evdev scancode 30 = AC01); the table below is in
// Linux evdev terms.
func XKBNameForLinuxScancode(code uint32) (string, bool) {
	name, ok := evdevToXKB[code]
	return name, ok
}

// LinuxScancodeForXKBName is the inverse — exposed primarily so
// tests can spell keystrokes in their natural mnemonic form.
func LinuxScancodeForXKBName(name string) (uint32, bool) {
	for code, n := range evdevToXKB {
		if n == name {
			return code, true
		}
	}
	return 0, false
}

// evdevToXKB maps Linux evdev scancodes to xkb keycode names.
// Source: /usr/share/X11/xkb/keycodes/evdev (X11 convention adds 8;
// we strip that). Coverage is the alphanumeric + punctuation block;
// keys outside this set (function keys, media, modifiers) aren't
// touched by the keymap layer in any realistic flexkb stack.
var evdevToXKB = map[uint32]string{
	// Number row
	1:  "ESC",
	2:  "AE01", // 1 !
	3:  "AE02", // 2 @
	4:  "AE03", // 3 #
	5:  "AE04", // 4 $
	6:  "AE05", // 5 %
	7:  "AE06", // 6 ^
	8:  "AE07", // 7 &
	9:  "AE08", // 8 *
	10: "AE09", // 9 (
	11: "AE10", // 0 )
	12: "AE11", // - _
	13: "AE12", // = +
	14: "BKSP",
	// Top letter row
	15: "TAB",
	16: "AD01", // q
	17: "AD02", // w
	18: "AD03", // e
	19: "AD04", // r
	20: "AD05", // t
	21: "AD06", // y
	22: "AD07", // u
	23: "AD08", // i
	24: "AD09", // o
	25: "AD10", // p
	26: "AD11", // [ {
	27: "AD12", // ] }
	28: "RTRN",
	// Home row
	29: "LCTL",
	30: "AC01", // a
	31: "AC02", // s
	32: "AC03", // d
	33: "AC04", // f
	34: "AC05", // g
	35: "AC06", // h
	36: "AC07", // j
	37: "AC08", // k
	38: "AC09", // l
	39: "AC10", // ; :
	40: "AC11", // ' "
	41: "TLDE", // ` ~
	42: "LFSH",
	43: "BKSL", // \ |
	// Bottom letter row
	44: "AB01", // z
	45: "AB02", // x
	46: "AB03", // c
	47: "AB04", // v
	48: "AB05", // b
	49: "AB06", // n
	50: "AB07", // m
	51: "AB08", // , <
	52: "AB09", // . >
	53: "AB10", // / ?
	54: "RTSH",
	// Modifier / space
	56: "LALT",
	57: "SPCE",
	58: "CAPS",
	86: "LSGT", // < > (ISO-only key between LFSH and AB01)
	100: "RALT",
}
