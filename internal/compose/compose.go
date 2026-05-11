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

// Result carries the composed layout plus any non-fatal warnings raised
// during composition (e.g. transformation referencing a key the physical
// doesn't have).
type Result struct {
	Layout   model.ComposedLayout
	Warnings []string
}

// Compose resolves a LayoutSpec against the data root. It loads the named
// physical, transformation, additions and substitutions, then merges them.
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
	return ComposeFromPartsWithSubs(spec, phys, trans, adds, subs), nil
}

// ComposeFromParts is the no-substitutions form, kept for the
// pre-substitution callers and tests. New callers should prefer
// ComposeFromPartsWithSubs.
func ComposeFromParts(spec model.LayoutSpec, phys model.Physical, trans model.Transformation, adds []model.Addition) Result {
	return ComposeFromPartsWithSubs(spec, phys, trans, adds, nil)
}

// ComposeFromPartsWithSubs does the actual merge once parts have been
// resolved. Split out so tests can drive composition with in-memory data.
func ComposeFromPartsWithSubs(spec model.LayoutSpec, phys model.Physical, trans model.Transformation, adds []model.Addition, subs []model.Substitution) Result {
	r := Result{}
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

	// Stage 1: seed from transformation. Keys not on the physical are
	// silently dropped if they're known cross-physical "extension" keys
	// (LSGT on ISO, AB11/AE13 on JIS). Warn for the rest — that's the case
	// that catches typos like ADO1 (zero vs O).
	for k, sym := range trans.Keys {
		if !keyAllowed[k] {
			if !extensionKeys[k] {
				r.Warnings = append(r.Warnings, fmt.Sprintf("transformation %q assigns key %s not on physical %s — dropped", trans.Name, k, phys.Name))
			}
			continue
		}
		out.Symbols[k] = model.KeySymbols{Levels: append([]string(nil), sym.Levels...)}
	}

	// Stage 2: apply additions in order.
	for _, a := range adds {
		for k, overlay := range a.Overlays {
			if !keyAllowed[k] {
				if !extensionKeys[k] {
					r.Warnings = append(r.Warnings, fmt.Sprintf("addition %q overlays key %s not on physical %s — dropped", a.Name, k, phys.Name))
				}
				continue
			}
			cur := out.Symbols[k]
			merged := mergeLevels(cur.Levels, overlay.Levels)
			out.Symbols[k] = model.KeySymbols{Levels: merged}
		}
		out.Includes = append(out.Includes, a.Includes...)
	}

	// Stage 3: apply substitutions as a final character-level pass. Each
	// substitution rewrites symbols in-place; later substitutions see the
	// already-rewritten values, allowing chains (e.g. apply Latin→Cyrillic
	// then a Cyrillic-respelling).
	if len(subs) > 0 {
		for k, sym := range out.Symbols {
			newLevels := make([]string, len(sym.Levels))
			for i, lv := range sym.Levels {
				newLevels[i] = applySubs(lv, subs)
			}
			out.Symbols[k] = model.KeySymbols{Levels: newLevels}
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
