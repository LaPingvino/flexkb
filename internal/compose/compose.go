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

// Result carries the composed layout plus any non-fatal warnings raised
// during composition (e.g. transformation referencing a key the physical
// doesn't have).
type Result struct {
	Layout   model.ComposedLayout
	Warnings []string
}

// Compose resolves a LayoutSpec against the data root. It loads the named
// physical, transformation and additions, then merges them.
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
	return ComposeFromParts(spec, phys, trans, adds), nil
}

// ComposeFromParts does the actual merge once parts have been resolved.
// Split out so tests can drive composition with in-memory data.
func ComposeFromParts(spec model.LayoutSpec, phys model.Physical, trans model.Transformation, adds []model.Addition) Result {
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

	// Stage 1: seed from transformation.
	for k, sym := range trans.Keys {
		if !keyAllowed[k] {
			r.Warnings = append(r.Warnings, fmt.Sprintf("transformation %q assigns key %s not on physical %s — dropped", trans.Name, k, phys.Name))
			continue
		}
		out.Symbols[k] = model.KeySymbols{Levels: append([]string(nil), sym.Levels...)}
	}

	// Stage 2: apply additions in order.
	for _, a := range adds {
		for k, overlay := range a.Overlays {
			if !keyAllowed[k] {
				r.Warnings = append(r.Warnings, fmt.Sprintf("addition %q overlays key %s not on physical %s — dropped", a.Name, k, phys.Name))
				continue
			}
			cur := out.Symbols[k]
			merged := mergeLevels(cur.Levels, overlay.Levels)
			out.Symbols[k] = model.KeySymbols{Levels: merged}
		}
		out.Includes = append(out.Includes, a.Includes...)
	}

	r.Layout = out
	return r
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
