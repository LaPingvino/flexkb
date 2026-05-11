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

import "strings"

// LetterOverlay is the value type for Addition.LetterOverlays. It pairs a
// per-level overlay with an optional ordered list of fallback level-1
// tokens to try if the primary letter isn't present in the base. This
// lets an addition specify "rough fit" behaviour on uncommon bases —
// e.g., Portuguese ç wants to land on 'c', but if a base has no 'c' at
// level 1, it can fall back to "comma" (visual cognate — ç ≈ c + cedilla,
// and comma's hook resembles cedilla). First match wins.
type LetterOverlay struct {
	Levels   []string `yaml:"levels,flow"`
	Fallback []string `yaml:"fallback,omitempty,flow"`
	// Priority is "high", "" (normal/default), or "low". Within an
	// addition, high-priority overlays apply first so they get first dibs
	// on the keys they target. Low-priority overlays apply last; if their
	// match key has already been claimed by a higher-priority overlay
	// from the same addition, they silently yield (no warning, layout
	// stays small and predictable). "Drop on poor fit" is implicit:
	// low-priority overlays with no primary or fallback match just don't
	// land anywhere.
	Priority string `yaml:"priority,omitempty"`
}

// PriorityRank maps the Priority string to a sortable int: high → 2,
// default ("") → 1, low → 0. Used by compose to order overlay
// application across all additions in a layout spec.
func (lo LetterOverlay) PriorityRank() int {
	switch strings.ToLower(lo.Priority) {
	case "high":
		return 2
	case "low":
		return 0
	default:
		return 1
	}
}

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
	// Script tags the writing system at level 1 — "latin", "cyrillic",
	// "greek", "arabic", "hebrew", "korean", "thai", etc. Empty defaults
	// to "latin". The matrix sanity test uses this to skip
	// transformation × addition pairs whose scripts don't match.
	Script string `yaml:"script,omitempty"`
	// Keys maps an XKB key code to the (typically 2-level) base symbol list.
	Keys map[string]KeySymbols `yaml:"keys"`
}

// Substitution is a character-level replacement table applied as a final
// pass over the composed layout. The classic use case is "Russian phonetic":
// take any Latin transformation (QWERTY/Dvorak/Colemak) and substitute each
// Latin letter for its phonetic Cyrillic cognate — a→а, b→б, etc. — so
// "Russian phonetic" works as a derivative of any base, not as a hand-coded
// per-base file. Same trick works for Greek, Bulgarian, Hebrew phonetic
// layouts.
//
// Substitutions are deliberately symbol-token-exact (not regex / substring):
// only whole-symbol matches like "a" → "Cyrillic_a" are replaced. Multi-
// letter names like "adiaeresis" or "Aacute" pass through unchanged so they
// don't get mangled when stacked with an "intl" Addition.
//
// Use the inverse direction by prefixing the substitution name with "~" in
// a LayoutSpec — e.g. "~latin-cyrillic-phonetic" maps Cyrillic_a → a, which
// lets a Russian-trained user get a Latin-via-ЙЦУКЕН-positions layout.
type Substitution struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description,omitempty"`
	// Map is source-symbol → target-symbol. Apply forward by default.
	Map map[string]string `yaml:"map"`
}

// Inverse returns a substitution with source/target swapped. If two source
// symbols map to the same target the result is non-deterministic; that's
// usually OK in practice (e.g. both 'v' and 'w' map to Cyrillic_ve, but
// inverting back lands you on one of them, which is fine for muscle-memory
// derivative layouts).
func (s Substitution) Inverse() Substitution {
	inv := make(map[string]string, len(s.Map))
	for k, v := range s.Map {
		inv[v] = k
	}
	return Substitution{
		Name:        s.Name + " (inverse)",
		Description: "inverse of " + s.Name,
		Map:         inv,
	}
}

// Addition overlays extra symbol levels (AltGr characters, dead keys) on top
// of an already-transformed layout. Two overlay styles are supported:
//
//   - Overlays (position-based): map specific XKB key codes to level lists.
//     Right when the user expects a glyph on a specific physical key — German
//     umlauts on the AC10/AC11/AD11 positions, Polish layout on AltGr+'a'-
//     position, etc.
//
//   - LetterOverlays (letter-following): map letter symbols to level lists
//     that should appear on whichever key currently holds that letter at
//     level 1. This is what xkb's us(intl), us(dvorak-intl), workman-intl,
//     etc. all do — accented forms of 'q' go wherever 'q' happens to live,
//     regardless of the underlying Latin transformation. ONE letter-overlay
//     definition replaces N hand-written transformation-specific variants.
//
// Empty-string levels are pass-through (don't overwrite). Overlays apply
// before LetterOverlays so position-specific patches take precedence.
type Addition struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description,omitempty"`
	// Scripts lists the writing systems this addition is meaningful on
	// (typically just one — "latin" for intl, "cyrillic" for
	// russian-phonetic-extras). Empty = "latin". The matrix sanity test
	// uses this to skip combinations like vietnamese-tone × ycuken that
	// would just produce no-op warnings. Use ["any"] to opt out.
	Scripts []string `yaml:"scripts,omitempty"`
	// Overlays maps an XKB key code to the per-level override list.
	Overlays map[string]KeySymbols `yaml:"overlays,omitempty"`
	// LetterOverlays maps a level-1 symbol token (e.g. "a", "q") to per-
	// level overrides applied to whichever physical key holds that letter
	// at level 1 after the transformation. Lookup is case-insensitive at
	// the key but case-sensitive in the values, so you can express
	// distinct shifted forms.
	//
	// Edge case — Turkish dotted i vs. dotless ı:
	// xkb has separate keysyms for the dotted and dotless forms ("i" /
	// "I" / "Iabovedot" / "idotless"), so this index keys them
	// distinctly. A `letter_overlays: { i: ... }` entry lands only on
	// keys whose level-1 is "i" (the dotted one). Turkish-F's ı key
	// (idotless) is untouched unless you write a separate entry for it.
	// If you want Turkish-style case pairing (i ↔ İ) on top of a non-
	// Turkish transformation, ship a small addition that sets level 2
	// of the 'i' overlay to Iabovedot — see data/additions/turkish-i-pair
	// for a worked example.
	LetterOverlays map[string]LetterOverlay `yaml:"letter_overlays,omitempty"`
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
	Physical       string   `yaml:"physical,omitempty"`
	Transformation string   `yaml:"transformation,omitempty"`
	Additions      []string `yaml:"additions,omitempty"`
	// Substitutions are applied AFTER additions. Prefix a name with "~" to
	// apply the inverse direction (target→source). Substitutions chain in
	// listed order, so you can stack e.g. [latin-cyrillic-phonetic,
	// some-cyrillic-respelling].
	Substitutions []string `yaml:"substitutions,omitempty"`
	// Passthrough marks this variant as "preserve upstream verbatim" —
	// at build time the flexkb build step reads the corresponding
	// xkb_symbols block from the source xkb tree and embeds its raw text
	// instead of composing from modular pieces. This is the map slot for
	// "we know this variant exists, we just haven't decomposed it yet."
	// A variant cannot be both Passthrough and have a composition; the
	// builder picks Passthrough first.
	Passthrough bool `yaml:"passthrough,omitempty"`
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
