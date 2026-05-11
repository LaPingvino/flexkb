// Package model defines the data types that describe a keyboard layout as
// three independent modules: a Physical layout (which keys exist), a
// Transformation (how letters/punctuation are arranged on those keys), and
// Additions (extra symbol levels, dead keys, national characters, modifier
// overrides). A finished xkb layout is the composition of one Physical, one
// Transformation, and zero or more Additions.
//
// The core promise of flexkb is: given N physical layouts, M transformations,
// and K additions, the user can generate N*M*K variants from N+M+K source
// files.
package model

// KeySymbols holds the per-level symbol assignments for a single physical key.
// Index 0 is the base (unmodified), 1 is shifted, 2 is level3 (typically
// AltGr), 3 is level3+shift. An empty string at a level means "leave whatever
// the previous composition stage put there" — this lets Additions overlay one
// level without clobbering the rest.
type KeySymbols struct {
	Levels []string `yaml:"levels,flow"`
}

// Physical describes a physical keyboard shell: which key codes are present.
// It carries no symbol information beyond key existence. ANSI (104 keys),
// ISO (105 keys with extra LSGT), and JIS (109 keys with additional keys
// like AB11/MUHE/HENK) are the canonical examples.
type Physical struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description,omitempty"`
	// Keys is the ordered list of XKB key codes (e.g. TLDE, AE01..AE12,
	// AD01..AD12, AC01..AC11, BKSL, LSGT, AB01..AB10) present on this shell.
	Keys []string `yaml:"keys,flow"`
}

// Transformation defines the *base* level-1/level-2 symbol assignment for
// each key. QWERTY, QWERTZ, AZERTY, Dvorak, Colemak, Workman, BÉPO etc. are
// all transformations: each is a permutation of the base alphabetic and
// punctuation keys.
//
// A transformation only needs to specify keys it actually assigns. Keys
// present in the Physical but not in the transformation produce a warning at
// compose time so you notice missing definitions.
type Transformation struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description,omitempty"`
	// Keys maps an XKB key code to the (typically 2-level) base symbol list.
	Keys map[string]KeySymbols `yaml:"keys"`
}

// Addition overlays extra symbol levels (AltGr characters, dead keys) on top
// of an already-transformed layout. Examples: us-intl dead keys, Polish
// AltGr-ą/ę/ć/etc, German umlauts on AC10/AC11/AD11.
//
// An overlay can specify any subset of levels. Empty-string levels are
// pass-through — they don't overwrite the underlying value. To extend a
// 2-level base to 4 levels, set levels 2 and 3 only.
type Addition struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description,omitempty"`
	// Overlays maps an XKB key code to the per-level override list.
	Overlays map[string]KeySymbols `yaml:"overlays,omitempty"`
	// Includes are raw xkb `include "..."` lines appended to the symbols
	// block — e.g. `level3(ralt_switch)` to enable AltGr.
	Includes []string `yaml:"includes,omitempty"`
}

// LayoutSpec is a recipe for one named output variant: pick a physical, a
// transformation, a list of additions, set a display name. The compose
// package turns this into a ComposedLayout.
type LayoutSpec struct {
	// Name is the xkb variant identifier (e.g. "basic", "dvorak", "intl").
	Name string `yaml:"name"`
	// Description is the human-readable name placed in xkb's `name[Group1]`
	// (e.g. "English (Dvorak)").
	Description string `yaml:"description"`
	// Physical, Transformation, Additions reference data files by their
	// base name (e.g. "ansi", "dvorak", "intl").
	Physical       string   `yaml:"physical"`
	Transformation string   `yaml:"transformation"`
	Additions      []string `yaml:"additions,omitempty"`
	// Default marks this variant as the file-level default (xkb syntax:
	// `default partial alphanumeric_keys`).
	Default bool `yaml:"default,omitempty"`
}

// LayoutFile groups a set of variants into one output xkb symbols file.
// Each xkb symbols file in /usr/share/X11/xkb/symbols (e.g. `us`, `de`)
// contains multiple variants; this is the flexkb-side equivalent.
type LayoutFile struct {
	// File is the xkb file name (e.g. "us", "de", "pl").
	File string `yaml:"file"`
	// Variants is the ordered list of variants in the file.
	Variants []LayoutSpec `yaml:"variants"`
}

// ComposedLayout is the result of resolving a LayoutSpec: a fully-populated
// per-key symbol table plus any include lines.
type ComposedLayout struct {
	Name        string
	Description string
	Default     bool
	// Keys preserves the order from the Physical layout.
	Keys     []string
	Symbols  map[string]KeySymbols
	Includes []string
}
