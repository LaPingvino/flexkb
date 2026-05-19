// Package validate implements the compositional validation step
// from docs/DESIGN-IME.md: walk a layout recipe's full layer stack,
// compute the produced character set, compare it against the
// target locale's required set, and report any gaps with actionable
// suggestions for which OTHER layers would supply each missing char.
//
// This is the matrix sanity test taken vertical. The existing
// matrix_test.go filters incompatible *pairs* via script tags; this
// package answers the stronger question "is this stack COMPLETE for
// the user's intended locale?" and tells you what to add if not.
package validate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lapingvino/flexkb/internal/compose"
	"github.com/lapingvino/flexkb/internal/coverage"
	"github.com/lapingvino/flexkb/internal/model"
)

// Report summarises one (variant, locale) validation pass.
type Report struct {
	File     string
	Variant  string
	Locale   string
	Coverage float64
	Total    int
	Missing  []MissingItem
}

// MissingItem is one character the locale requires but the layer
// stack doesn't produce, paired with the layers that WOULD supply
// it if added to the recipe.
type MissingItem struct {
	Char        string
	Suggestions []Suggestion
}

// Suggestion names a layer that would close one gap. Kind indicates
// where in the stack the layer attaches (so the recipe diff is
// unambiguous: an addition vs. a substitution lands in different
// LayoutSpec fields).
type Suggestion struct {
	Name string
	Kind string // "addition", "substitution", "compose", "inputmethod"
}

// ValidateVariant runs the full pipeline for one variant and returns
// one Report per locale the layout file targets. If the layout file
// doesn't declare locales, falls back to the locale table's
// inferred mapping (locales.yaml's LocalesFor logic).
//
// Empty Reports slice means the variant either has no locale to
// check against (e.g. a phonetic non-Latin variant with no locale
// metadata) or compose failed; check err first.
func ValidateVariant(root model.DataRoot, lf model.LayoutFile, spec model.LayoutSpec) ([]Report, error) {
	res, err := compose.Compose(root, spec)
	if err != nil {
		return nil, err
	}
	produced := coverage.CollectChars(asSymbolMap(res.Layout))

	tab, err := loadLocales(root)
	if err != nil {
		return nil, err
	}

	locales := localesForVariant(lf, spec, tab)
	if len(locales) == 0 {
		return nil, nil
	}

	// Build the layer→produced-chars index once; reuse across each
	// missing-char query.
	idx, err := buildSuggestionIndex(root)
	if err != nil {
		return nil, err
	}
	// Layers already in this recipe are excluded from suggestions
	// (no point telling the user to add what they already have).
	have := map[layerKey]bool{}
	for _, n := range spec.Additions {
		have[layerKey{Kind: "addition", Name: n}] = true
	}
	for _, n := range spec.Substitutions {
		have[layerKey{Kind: "substitution", Name: strings.TrimPrefix(n, "~")}] = true
	}
	for _, n := range spec.Compose {
		have[layerKey{Kind: "compose", Name: n}] = true
	}

	var out []Report
	for _, code := range locales {
		var loc coverage.Locale
		found := false
		for _, l := range tab.Locales {
			if l.Code == code {
				loc = l
				found = true
				break
			}
		}
		if !found {
			continue
		}
		ana := coverage.Analyze(produced, loc.Code, loc.Name, loc.Chars)
		r := Report{
			File:     lf.File,
			Variant:  spec.Name,
			Locale:   loc.Code,
			Coverage: ana.Coverage,
			Total:    ana.Total,
		}
		for _, ch := range ana.Missing {
			r.Missing = append(r.Missing, MissingItem{
				Char:        ch,
				Suggestions: idx.suggestionsFor(runeOf(ch), have),
			})
		}
		out = append(out, r)
	}
	return out, nil
}

// asSymbolMap unwraps ComposedLayout.Symbols into the shape
// coverage.CollectChars expects (map[key]→Levels list).
func asSymbolMap(layout model.ComposedLayout) map[string][]string {
	out := make(map[string][]string, len(layout.Symbols))
	for k, sym := range layout.Symbols {
		out[k] = sym.Levels
	}
	return out
}

// runeOf extracts the first rune from a single-character missing-
// list entry. coverage.Analyze emits each missing char as its own
// string, so the first rune is the only one.
func runeOf(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// loadLocales reads and parses data/locales.yaml from the data
// root. Returns an empty table (no error) when locales.yaml is
// absent, so validate can run on a flexkb tree without locale
// metadata.
func loadLocales(root model.DataRoot) (coverage.LocaleTable, error) {
	b, _, err := root.ReadTopLevel("locales.yaml")
	if err != nil {
		return coverage.LocaleTable{}, nil
	}
	return coverage.LoadTable(b)
}

// localesForVariant returns the locale codes a variant should be
// validated against. The LayoutSpec's LocaleFill overrides; absent
// that, the LocaleTable's LocalesFor(file) inference applies.
func localesForVariant(lf model.LayoutFile, spec model.LayoutSpec, tab coverage.LocaleTable) []string {
	if len(spec.LocaleFill) > 0 {
		return spec.LocaleFill
	}
	return tab.LocalesFor(lf.File)
}

// layerKey distinguishes "intl" the addition from "intl" the
// substitution etc. — names aren't globally unique across layer
// kinds.
type layerKey struct {
	Kind string
	Name string
}

// suggestionIndex maps each producible character to the layers
// that produce it. Computed once per validate run and reused
// across every missing-char lookup.
type suggestionIndex struct {
	// byChar[r] = list of layers that emit r somewhere in their
	// output. Layers appear in stable order (alpha by name within
	// each kind).
	byChar map[rune][]Suggestion
}

func (idx *suggestionIndex) suggestionsFor(r rune, exclude map[layerKey]bool) []Suggestion {
	var out []Suggestion
	for _, s := range idx.byChar[r] {
		if exclude[layerKey{Kind: s.Kind, Name: s.Name}] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// buildSuggestionIndex walks all additions, substitutions, and
// compose chains in the data root, indexing each by the characters
// it emits. Input methods aren't indexed yet — they emit commits
// dynamically and need an evaluator pass to know their producible
// set; punt to the future.
func buildSuggestionIndex(root model.DataRoot) (*suggestionIndex, error) {
	idx := &suggestionIndex{byChar: map[rune][]Suggestion{}}

	// Additions
	addNames, err := root.ListNames("additions")
	if err == nil {
		var names []string
		for n := range addNames {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a, err := root.Addition(n)
			if err != nil {
				continue
			}
			for _, sym := range a.Overlays {
				idx.indexLevels(sym.Levels, Suggestion{Name: n, Kind: "addition"})
			}
			for _, lo := range a.LetterOverlays {
				idx.indexLevels(lo.Levels, Suggestion{Name: n, Kind: "addition"})
			}
		}
	}

	// Substitutions
	subNames, err := root.ListNames("substitutions")
	if err == nil {
		var names []string
		for n := range subNames {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s, err := root.Substitution(n)
			if err != nil {
				continue
			}
			for _, target := range s.Map {
				idx.indexToken(target, Suggestion{Name: n, Kind: "substitution"})
			}
		}
	}

	// Compose chains
	composeNames, err := root.ListNames("compose")
	if err == nil {
		var names []string
		for n := range composeNames {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c, err := root.Compose(n)
			if err != nil {
				continue
			}
			sug := Suggestion{Name: n, Kind: "compose"}
			for _, seq := range c.Sequences {
				for _, cp := range seq.Output {
					if r, ok := decodeUEscape(cp); ok {
						idx.add(r, sug)
					}
				}
			}
		}
	}

	return idx, nil
}

// indexLevels walks a list of symbol tokens (one xkb level's
// content or a letter-overlay's per-level list) and indexes each
// decodable rune.
func (idx *suggestionIndex) indexLevels(levels []string, sug Suggestion) {
	for _, t := range levels {
		idx.indexToken(t, sug)
	}
}

// indexToken decodes one symbol token and indexes the resulting
// rune. Tokens that don't resolve to a single rune (NoSymbol,
// dead_*, unknown tokens) are ignored — they can't satisfy a
// missing-character query.
func (idx *suggestionIndex) indexToken(token string, sug Suggestion) {
	r, ok := coverage.Decode(token)
	if !ok {
		return
	}
	idx.add(r, sug)
}

func (idx *suggestionIndex) add(r rune, sug Suggestion) {
	// Dedup: a layer only listed once per character even if it
	// produces it at multiple positions/levels.
	for _, existing := range idx.byChar[r] {
		if existing == sug {
			return
		}
	}
	idx.byChar[r] = append(idx.byChar[r], sug)
}

// decodeUEscape parses "U0915" into the rune it represents.
// Duplicated from inputmethod.compose (different package, same
// trivial logic; not worth promoting to a shared helper yet).
func decodeUEscape(s string) (rune, bool) {
	if len(s) < 2 || (s[0] != 'U' && s[0] != 'u') {
		return 0, false
	}
	var v rune
	for _, c := range s[1:] {
		var d rune
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		v = v*16 + d
	}
	return v, true
}

// FormatReport renders a Report as a human-readable multi-line
// string. Used by the CLI and by tests that want a stable
// pretty-printed comparison form.
func FormatReport(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s(%s) for locale %s — %d/%d covered (%.0f%%)\n",
		r.File, r.Variant, r.Locale, r.Total-len(r.Missing), r.Total, r.Coverage*100)
	if len(r.Missing) == 0 {
		b.WriteString("  ✓ complete\n")
		return b.String()
	}
	for _, m := range r.Missing {
		fmt.Fprintf(&b, "  missing %s", m.Char)
		if len(m.Suggestions) == 0 {
			b.WriteString("  (no layer in data/ produces this)\n")
			continue
		}
		b.WriteString("  — add one of:")
		for i, s := range m.Suggestions {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, " %s/%s", s.Kind, s.Name)
		}
		b.WriteString("\n")
	}
	return b.String()
}
