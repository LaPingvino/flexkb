package compose

import (
	"sort"

	"github.com/lapingvino/flexkb/internal/coverage"
	"github.com/lapingvino/flexkb/internal/model"
)

// ApplyLocaleFill runs the post-composition pass that tries to place
// each character a primary locale needs but the composed layout doesn't
// produce yet, into an empty slot somewhere reasonable. The goal isn't
// perfection — it's to catch the "polite nudge cascade dropped Ç on
// AZERTY+intl" case automatically without the user having to author a
// be-specific addition.
//
// Algorithm per missing character:
//
//   1. Resolve the character to an xkb keysym name (case-aware, via
//      coverage.EncodeRune). Fall back to U-escape on unknown runes.
//
//   2. Build a candidate-key list, in this order:
//        a. The key whose level-1 is the case-folded lowercase of the
//           missing character (Ccedilla → near 'c'; works on any base
//           because we only need 'c' at L1).
//        b. Any key that already produces the lowercase pair of the
//           missing character anywhere (so Ccedilla lands on the same
//           key as ccedilla — typically AE09 on AZERTY).
//        c. Any other key, in physical order, with at least one
//           non-empty level (avoids dumping the locale fill on a
//           completely unused key like NumpadEqual).
//        d. Any key still entirely empty (last resort — better there
//           than nowhere).
//
//   3. For each candidate, walk slots in preference order L4 → L3 → L2
//      and place at the first empty one. L4 first because it's the
//      least-trafficked AltGr+Shift slot; L2 only as a last resort
//      because clobbering Shift-state is more user-visible.
//
// Returns the number of characters successfully placed; characters
// that couldn't fit are accumulated into r.Warnings. Sources are
// updated with stage="locale-fill" and module=<locale code>.
func ApplyLocaleFill(r *Result, missing []rune, locale string) int {
	if r == nil || len(missing) == 0 {
		return 0
	}
	placed := 0
	// Build a snapshot of which exact characters (case-preserved) the
	// layout currently produces. Locale-fill cares about both cases
	// independently — having lowercase ç doesn't mean Ç is reachable.
	produced := producedExact(r)
	// Sort missing by "anchor quality" so case-paired characters with
	// a natural home win the easy slots over orphan characters that
	// would land anywhere. Without this, alphabetic iteration eats
	// the empty slots with low-anchor chars (Ÿ, Û) before the user-
	// essential Ç even gets a chance. Score:
	//   100  lowercase counterpart is at L1 of some key (best anchor)
	//    50  lowercase counterpart appears anywhere on layout
	//     0  no anchor — likely lands on a generic empty slot
	// Stable sort preserves caller-provided order within a score
	// bucket so locales.yaml curators can still hint within a bucket.
	missing = sortByAnchorQuality(r, missing)
	for _, ch := range missing {
		if produced[ch] {
			continue
		}
		ks, ok := coverage.EncodeRune(ch)
		if !ok {
			r.Warnings = append(r.Warnings, "locale-fill: no keysym for "+string(ch))
			continue
		}
		k, slot, found := findLocaleFillSlot(r, ch, ks)
		if !found {
			r.Warnings = append(r.Warnings, "locale-fill: no slot for "+string(ch)+" ("+ks+")")
			continue
		}
		// Ensure the levels slice is long enough.
		cur := r.Layout.Symbols[k].Levels
		for len(cur) <= slot {
			cur = append(cur, "")
		}
		cur[slot] = ks
		r.Layout.Symbols[k] = model.KeySymbols{Levels: cur}
		// Mirror Sources.
		src := r.Sources[k]
		for len(src) <= slot {
			src = append(src, LevelSource{})
		}
		src[slot] = LevelSource{Stage: "locale-fill", Module: locale}
		r.Sources[k] = src
		// Refresh the produced set so case pairs in the same pass see
		// the newly-placed character.
		produced[ch] = true
		placed++
	}
	return placed
}

// producedExact returns the exact-case rune set the layout currently
// produces. Differs from coverage.CollectChars (which case-folds) —
// locale-fill needs to know if uppercase Ç is missing even when
// lowercase ç is present, so two passes can place both forms.
func producedExact(r *Result) map[rune]bool {
	out := map[rune]bool{}
	for _, sym := range r.Layout.Symbols {
		for _, lv := range sym.Levels {
			if rn, ok := coverage.Decode(lv); ok {
				out[rn] = true
			}
		}
	}
	return out
}

// sortByAnchorQuality reorders missing so characters whose lowercase
// counterpart has a natural anchor key come first. Anchor scoring:
//
//   100  lowercase pair lives at L1 of some key  (best home: shift-
//        access is already wired, locale-fill just needs to put the
//        uppercase form on a near slot)
//    50  lowercase pair lives anywhere on layout (a related key
//        exists, even if shift-access wouldn't be natural)
//     0  no related key — fallback to "any empty slot"
//
// Stable sort preserves the locales.yaml ordering within a score
// bucket, so curators can still hint relative priority between
// equal-anchor chars.
func sortByAnchorQuality(r *Result, missing []rune) []rune {
	hasAtL1 := map[rune]bool{}
	hasAnywhere := map[rune]bool{}
	for _, k := range r.Layout.Keys {
		sym, ok := r.Layout.Symbols[k]
		if !ok || len(sym.Levels) == 0 {
			continue
		}
		if r1, ok := coverage.Decode(sym.Levels[0]); ok {
			hasAtL1[lowerRune(r1)] = true
			hasAnywhere[lowerRune(r1)] = true
		}
		for _, lv := range sym.Levels[1:] {
			if rn, ok := coverage.Decode(lv); ok {
				hasAnywhere[lowerRune(rn)] = true
			}
		}
	}
	type scored struct {
		idx   int
		score int
		ch    rune
	}
	items := make([]scored, len(missing))
	for i, ch := range missing {
		lr := lowerRune(ch)
		s := 0
		switch {
		case hasAtL1[lr]:
			s = 100
		case hasAnywhere[lr]:
			s = 50
		}
		items[i] = scored{idx: i, score: s, ch: ch}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].idx < items[j].idx
	})
	out := make([]rune, len(items))
	for i, it := range items {
		out[i] = it.ch
	}
	return out
}

// findLocaleFillSlot picks a (key, slot) for placing the missing
// character. Returns ok=false if every candidate key is full to L4.
func findLocaleFillSlot(r *Result, ch rune, keysym string) (string, int, bool) {
	cand := buildCandidateKeys(r, ch, keysym)
	for _, k := range cand {
		levels := r.Layout.Symbols[k].Levels
		// Walk L4 first so locale-fill lands at AltGr+Shift before
		// clobbering anywhere more visible.
		for _, slot := range []int{3, 2, 1} {
			var existing string
			if slot < len(levels) {
				existing = levels[slot]
			}
			if existing == "" {
				return k, slot, true
			}
		}
	}
	return "", 0, false
}

// buildCandidateKeys returns the ordered list of keys to try. Anchor
// keys (related letter) come first, then partial-fit, then any-empty.
func buildCandidateKeys(r *Result, ch rune, keysym string) []string {
	wantRune := lowerRune(ch)
	// Index keys by the DECODED character of their levels — Decode
	// resolves both single-char tokens ("c") and named keysyms
	// ("ccedilla") into the same rune namespace. Without this the
	// match fails: byL1["ccedilla"] ≠ byL1["ç"], so a Ccedilla
	// placement would never find AE09 as an anchor.
	byL1 := map[rune][]string{}
	byAnyLevel := map[rune][]string{}
	for _, k := range r.Layout.Keys {
		sym, ok := r.Layout.Symbols[k]
		if !ok || len(sym.Levels) == 0 {
			continue
		}
		if r1, ok := coverage.Decode(sym.Levels[0]); ok {
			rl := lowerRune(r1)
			byL1[rl] = append(byL1[rl], k)
		}
		for _, lv := range sym.Levels {
			if rn, ok := coverage.Decode(lv); ok {
				byAnyLevel[lowerRune(rn)] = append(byAnyLevel[lowerRune(rn)], k)
			}
		}
	}
	added := map[string]bool{}
	var out []string
	push := func(k string) {
		if k == "" || added[k] {
			return
		}
		// Only consider keys with at least one empty slot.
		levels := r.Layout.Symbols[k].Levels
		emptySlot := false
		for _, slot := range []int{3, 2, 1} {
			var existing string
			if slot < len(levels) {
				existing = levels[slot]
			}
			if existing == "" {
				emptySlot = true
				break
			}
		}
		if !emptySlot {
			return
		}
		added[k] = true
		out = append(out, k)
	}
	// (a) keys whose L1 is the lowercase counterpart of the missing
	// character — Ccedilla → near a 'c' key, or on AZERTY where 'ç'
	// itself sits at L1 of AE09, that key matches too.
	for _, k := range byL1[wantRune] {
		push(k)
	}
	// (b) keys that produce the lowercase pair anywhere on the key
	// (so Ccedilla also considers AB07 where intl nudged ccedilla to
	// L4 on AZERTY).
	for _, k := range byAnyLevel[wantRune] {
		push(k)
	}
	// Suppress the unused-arg warning — keysym is logically what we
	// place, but candidate search is driven entirely by the lowercase
	// counterpart rune.
	_ = keysym
	// (c) keys with content but at least one empty slot, in physical
	// order. Stable so test expectations don't depend on map iteration.
	for _, k := range r.Layout.Keys {
		sym, ok := r.Layout.Symbols[k]
		if !ok || len(sym.Levels) == 0 {
			continue
		}
		push(k)
	}
	// (d) entirely-empty keys go last, also in physical order.
	for _, k := range r.Layout.Keys {
		sym, ok := r.Layout.Symbols[k]
		if !ok || len(sym.Levels) == 0 {
			push(k)
		}
	}
	// Sort within already-equivalent groups isn't needed — push
	// preserves the order each loop appends in, which is stable.
	_ = sort.Strings
	return out
}

func lowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return runeFoldLower(r)
}

// runeFoldLower is the case-fold equivalent of unicode.ToLower without
// pulling in unicode just for one call; we already use coverage which
// imports it, but the locale-fill path doesn't need anything fancier
// for European text.
func runeFoldLower(r rune) rune {
	// Latin-1 supplement uppercase block.
	if r >= 0xC0 && r <= 0xDE && r != 0xD7 {
		return r + 0x20
	}
	// Latin Extended-A: many pairs are even/odd (Aogonek/aogonek).
	if r >= 0x0100 && r <= 0x017F {
		if r%2 == 0 {
			return r + 1
		}
	}
	return r
}
