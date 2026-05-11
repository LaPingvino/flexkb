// Package compose merges a Physical + Transformation + Additions tuple into
// a ComposedLayout. The composition rules are:
//
//  1. The Physical layout defines the universe of keys. Anything not in the
//     Physical's key list is dropped (with a warning) — that's how, e.g.,
//     an ANSI variant of a layout that touches LSGT silently omits LSGT.
//  2. The Transformation seeds the base symbol map. For each key it lists,
//     all levels are set from the transformation. Keys the transformation
//     doesn't mention get an empty 2-level entry.
//  3. Each Addition is applied in order. For each (key, levels) overlay,
//     non-empty level strings overwrite the existing value at that level;
//     empty strings are pass-through. The output level count is
//     max(existing, overlay).
package compose

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lapingvino/flexkb/internal/model"
)

// extensionKeys is the set of XKB keycodes that are present on some physical
// shells but legitimately absent on others. A transformation or addition that
// defines these doesn't need to warn when composed onto a physical that
// doesn't have them — that's by design (e.g. an ISO transformation composed
// onto an ANSI shell drops LSGT).
var extensionKeys = map[string]bool{
	"LSGT": true, // ISO 105: extra key between Left Shift and Z
	"AB11": true, // JIS 109: ¥ / underscore key
	"AE13": true, // JIS 109: extra top-row key
}

// LevelSource records which composition stage and which specific
// module produced one level value on one key. Stages are the canonical
// four — transformation, position, letter, substitution — and Module
// is the file name of the contributing transformation / addition /
// substitution (e.g. "qwerty", "intl", "latin-cyrillic-phonetic").
// Used by the GUI inspector to show "level 3 came from `intl`" rather
// than just "letter overlay", and available to any caller that wants
// per-level provenance without re-running composition.
type LevelSource struct {
	Stage  string // "transformation" / "position" / "letter" / "substitution" / ""
	Module string // contributing module name; "" if Stage is also ""
}

// Result carries the composed layout plus any non-fatal warnings raised
// during composition (e.g. transformation referencing a key the physical
// doesn't have).
//
// Sources is a parallel map to Layout.Symbols: Sources[k][i] describes
// where Layout.Symbols[k].Levels[i] came from. Always populated.
type Result struct {
	Layout   model.ComposedLayout
	Warnings []string
	Sources  map[string][]LevelSource
}

// Compose resolves a LayoutSpec against the data root. It loads the named
// physical, transformation, additions and substitutions, then merges them.
//
// If spec.Autofill is set, the data root is also consulted for the
// pool of filler additions matching the requested categories. Fillers
// apply with empty-only merge semantics — they never override an
// existing value, only fill levels still empty after the main stages.
func Compose(root model.DataRoot, spec model.LayoutSpec) (Result, error) {
	phys, err := root.Physical(spec.Physical)
	if err != nil {
		return Result{}, fmt.Errorf("variant %q: %w", spec.Name, err)
	}
	trans, err := root.Transformation(spec.Transformation)
	if err != nil {
		return Result{}, fmt.Errorf("variant %q: %w", spec.Name, err)
	}
	adds := make([]model.Addition, 0, len(spec.Additions))
	for _, n := range spec.Additions {
		a, err := root.Addition(n)
		if err != nil {
			return Result{}, fmt.Errorf("variant %q: %w", spec.Name, err)
		}
		adds = append(adds, a)
	}
	subs := make([]model.Substitution, 0, len(spec.Substitutions))
	for _, n := range spec.Substitutions {
		s, err := root.Substitution(n)
		if err != nil {
			return Result{}, fmt.Errorf("variant %q: %w", spec.Name, err)
		}
		subs = append(subs, s)
	}
	var fillers []model.Addition
	if len(spec.Autofill) > 0 {
		all, err := root.ListFillers()
		if err != nil {
			return Result{}, fmt.Errorf("variant %q: autofill lookup: %w", spec.Name, err)
		}
		fillers = pickFillers(all, spec.Autofill)
	}
	return ComposeFromPartsFull(spec, phys, trans, adds, subs, fillers), nil
}

// pickFillers selects fillers whose Categories overlap with the
// requested set. A literal "*" in requested means "every filler".
// Result preserves the requested category order, so a user-controlled
// "typography first, math second" intent is honoured.
func pickFillers(all []model.Addition, requested []string) []model.Addition {
	wantAll := false
	want := map[string]bool{}
	for _, c := range requested {
		if c == "*" {
			wantAll = true
		}
		want[c] = true
	}
	if wantAll {
		return all
	}
	// Order fillers by requested-category order so "typography first"
	// applies before "math" — affects which filler wins when two
	// fillers both target the same empty level.
	seen := map[string]bool{}
	var out []model.Addition
	for _, c := range requested {
		for _, a := range all {
			if seen[a.Slug] {
				continue
			}
			for _, ac := range a.Categories {
				if ac == c {
					out = append(out, a)
					seen[a.Slug] = true
					break
				}
			}
		}
	}
	return out
}

// ComposeFromParts is the no-substitutions form, kept for the
// pre-substitution callers and tests. New callers should prefer
// ComposeFromPartsFull.
func ComposeFromParts(spec model.LayoutSpec, phys model.Physical, trans model.Transformation, adds []model.Addition) Result {
	return ComposeFromPartsFull(spec, phys, trans, adds, nil, nil)
}

// ComposeFromPartsWithSubs is the pre-autofill API, kept so existing
// in-memory callers don't break. New callers should prefer
// ComposeFromPartsFull which also handles the autofill pool.
func ComposeFromPartsWithSubs(spec model.LayoutSpec, phys model.Physical, trans model.Transformation, adds []model.Addition, subs []model.Substitution) Result {
	return ComposeFromPartsFull(spec, phys, trans, adds, subs, nil)
}

// ComposeFromPartsFull does the actual merge once parts have been
// resolved. Split out so tests can drive composition with in-memory data.
//
// Tracks per-level provenance into r.Sources alongside the composed
// symbols: every time a stage writes a non-empty value into a level,
// we record (stage, module) for that (key, level). Empty values
// pass through unchanged and so does their source record.
//
// Stages: transformation seed → positional addition overlays →
// letter overlays → substitutions → (optionally) autofill from the
// filler pool. Autofill uses empty-only-merge so it never overrides
// content from earlier stages.
func ComposeFromPartsFull(spec model.LayoutSpec, phys model.Physical, trans model.Transformation, adds []model.Addition, subs []model.Substitution, fillers []model.Addition) Result {
	r := Result{Sources: map[string][]LevelSource{}}
	out := model.ComposedLayout{
		Name:        spec.Name,
		Description: spec.Description,
		Default:     spec.Default,
		Keys:        append([]string(nil), phys.Keys...),
		Symbols:     map[string]model.KeySymbols{},
	}

	keyAllowed := map[string]bool{}
	for _, k := range phys.Keys {
		keyAllowed[k] = true
	}

	// writeLevels merges overlay levels into key k's current levels and
	// records (stage, module) for every level the overlay actually set.
	// Out-parameter style on r.Sources keeps the existing stages tidy.
	writeLevels := func(k string, overlayLevels []string, stage, module string) {
		cur := out.Symbols[k].Levels
		merged := mergeLevels(cur, overlayLevels)
		out.Symbols[k] = model.KeySymbols{Levels: merged}
		// Sources: extend to merged length, copy existing, mark levels
		// the overlay populated.
		curSrc := r.Sources[k]
		newSrc := make([]LevelSource, len(merged))
		copy(newSrc, curSrc) // existing sources preserved where overlay didn't write
		for i := 0; i < len(merged); i++ {
			ov := ""
			if i < len(overlayLevels) {
				ov = overlayLevels[i]
			}
			if ov != "" {
				newSrc[i] = LevelSource{Stage: stage, Module: module}
			}
		}
		r.Sources[k] = newSrc
	}

	// Stage 1: seed from transformation. We tag provenance with the
	// transformation's Slug (the file basename) rather than the YAML
	// `name:` field — that's what users pick in the GUI / layouts file,
	// so it's what makes most sense in the inspector.
	transSlug := trans.Slug
	if transSlug == "" {
		transSlug = trans.Name
	}
	for k, sym := range trans.Keys {
		if !keyAllowed[k] {
			if !extensionKeys[k] {
				r.Warnings = append(r.Warnings, fmt.Sprintf("transformation %q assigns key %s not on physical %s — dropped", trans.Name, k, phys.Name))
			}
			continue
		}
		levels := append([]string(nil), sym.Levels...)
		out.Symbols[k] = model.KeySymbols{Levels: levels}
		src := make([]LevelSource, len(levels))
		for i, v := range levels {
			if v != "" {
				src[i] = LevelSource{Stage: "transformation", Module: transSlug}
			}
		}
		r.Sources[k] = src
	}

	// Stage 2: positional addition overlays in declaration order.
	addSlug := func(a model.Addition) string {
		if a.Slug != "" {
			return a.Slug
		}
		return a.Name
	}
	for _, a := range adds {
		for k, overlay := range a.Overlays {
			if !keyAllowed[k] {
				if !extensionKeys[k] {
					r.Warnings = append(r.Warnings, fmt.Sprintf("addition %q overlays key %s not on physical %s — dropped", a.Name, k, phys.Name))
				}
				continue
			}
			writeLevels(k, overlay.Levels, "position", addSlug(a))
		}
		out.Includes = append(out.Includes, a.Includes...)
	}

	// Stage 2b: letter-following overlays. Priority + claimedRank exactly
	// as before; the only addition is the source tag.
	type entry struct {
		addName string
		letter  string
		overlay model.LetterOverlay
	}
	var allEntries []entry
	for _, a := range adds {
		for letter, ov := range a.LetterOverlays {
			allEntries = append(allEntries, entry{addSlug(a), letter, ov})
		}
	}
	if len(allEntries) > 0 {
		byLetter := map[string][]string{}
		for _, k := range out.Keys {
			sym, ok := out.Symbols[k]
			if !ok || len(sym.Levels) == 0 {
				continue
			}
			byLetter[strings.ToLower(sym.Levels[0])] = append(byLetter[strings.ToLower(sym.Levels[0])], k)
		}
		sort.SliceStable(allEntries, func(i, j int) bool {
			return allEntries[i].overlay.PriorityRank() > allEntries[j].overlay.PriorityRank()
		})
		claimedRank := map[string]int{}
		for _, e := range allEntries {
			keys := byLetter[strings.ToLower(e.letter)]
			if len(keys) == 0 {
				for _, fb := range e.overlay.Fallback {
					keys = byLetter[strings.ToLower(fb)]
					if len(keys) > 0 {
						break
					}
				}
			}
			if len(keys) == 0 {
				continue
			}
			rank := e.overlay.PriorityRank()
			for _, k := range keys {
				if prev, ok := claimedRank[k]; ok && rank < prev {
					continue
				}
				writeLevels(k, e.overlay.Levels, "letter", e.addName)
				if rank > claimedRank[k] {
					claimedRank[k] = rank
				}
			}
		}
	}

	// Stage 3: substitutions. If a sub rewrites a value, the level's
	// source becomes (substitution, sub.Name). Chained subs: the final
	// substitution that actually changed the value wins.
	if len(subs) > 0 {
		for k, sym := range out.Symbols {
			newLevels := make([]string, len(sym.Levels))
			src := r.Sources[k]
			if len(src) < len(sym.Levels) {
				// Defensive: extend if compose ever produced ragged sources.
				ext := make([]LevelSource, len(sym.Levels))
				copy(ext, src)
				src = ext
			}
			for i, lv := range sym.Levels {
				after, by := applySubsTraced(lv, subs)
				newLevels[i] = after
				if by != "" {
					src[i] = LevelSource{Stage: "substitution", Module: by}
				}
			}
			out.Symbols[k] = model.KeySymbols{Levels: newLevels}
			r.Sources[k] = src
		}
	}

	// Stage 4 (optional): autofill from filler pool. Empty-only merge —
	// never override a level that any earlier stage already populated.
	if len(fillers) > 0 {
		// Build the byLetter index again — letter overlays may have
		// changed level-1 values during stage 2b.
		byLetter := map[string][]string{}
		for _, k := range out.Keys {
			sym, ok := out.Symbols[k]
			if !ok || len(sym.Levels) == 0 {
				continue
			}
			byLetter[strings.ToLower(sym.Levels[0])] = append(byLetter[strings.ToLower(sym.Levels[0])], k)
		}
		applyFillerOverlay := func(k string, overlay []string, fillerSlug string) {
			cur := out.Symbols[k].Levels
			n := len(cur)
			if len(overlay) > n {
				n = len(overlay)
			}
			next := make([]string, n)
			copy(next, cur)
			curSrc := r.Sources[k]
			nextSrc := make([]LevelSource, n)
			copy(nextSrc, curSrc)
			for i := 0; i < n; i++ {
				var ov string
				if i < len(overlay) {
					ov = overlay[i]
				}
				var existing string
				if i < len(next) {
					existing = next[i]
				}
				if ov != "" && existing == "" {
					next[i] = ov
					nextSrc[i] = LevelSource{Stage: "autofill", Module: fillerSlug}
				}
			}
			// Trim trailing empties for symmetry with mergeLevels.
			for len(next) > 0 && next[len(next)-1] == "" {
				next = next[:len(next)-1]
				if len(nextSrc) > len(next) {
					nextSrc = nextSrc[:len(next)]
				}
			}
			out.Symbols[k] = model.KeySymbols{Levels: next}
			r.Sources[k] = nextSrc
		}
		for _, f := range fillers {
			fs := f.Slug
			if fs == "" {
				fs = f.Name
			}
			for k, ov := range f.Overlays {
				if !keyAllowed[k] {
					continue
				}
				applyFillerOverlay(k, ov.Levels, fs)
			}
			for letter, ov := range f.LetterOverlays {
				keys := byLetter[strings.ToLower(letter)]
				if len(keys) == 0 {
					for _, fb := range ov.Fallback {
						keys = byLetter[strings.ToLower(fb)]
						if len(keys) > 0 {
							break
						}
					}
				}
				for _, k := range keys {
					applyFillerOverlay(k, ov.Levels, fs)
				}
			}
			out.Includes = append(out.Includes, f.Includes...)
		}
	}

	r.Layout = out
	return r
}

func applySubs(sym string, subs []model.Substitution) string {
	if sym == "" {
		return sym
	}
	out := sym
	for _, s := range subs {
		if mapped, ok := s.Map[out]; ok {
			out = mapped
		}
	}
	return out
}

// applySubsTraced returns the rewritten value and the slug of the last
// substitution that actually changed it (empty if no substitution
// matched). Used by ComposeFromPartsWithSubs to record provenance.
// We prefer the file-basename slug ("~latin-cyrillic-phonetic") over
// the YAML name ("Latin → Cyrillic (phonetic)") so the GUI inspector
// matches what the user picks in the substitutions dropdown.
func applySubsTraced(sym string, subs []model.Substitution) (string, string) {
	if sym == "" {
		return sym, ""
	}
	out := sym
	by := ""
	for _, s := range subs {
		if mapped, ok := s.Map[out]; ok && mapped != out {
			out = mapped
			if s.Slug != "" {
				by = s.Slug
			} else {
				by = s.Name
			}
		}
	}
	return out, by
}

// mergeLevels overlays b on top of a. Result length is max(len(a), len(b)).
// At index i: empty b[i] (or missing) -> a[i]; non-empty b[i] -> b[i].
func mergeLevels(a, b []string) []string {
	n := max(len(a), len(b))
	out := make([]string, n)
	for i := 0; i < n; i++ {
		var av, bv string
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if bv != "" {
			out[i] = bv
		} else {
			out[i] = av
		}
	}
	// Trim trailing empties — no point emitting bare commas in xkb output.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
