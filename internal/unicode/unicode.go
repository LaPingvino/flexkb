// Package unicode parses UnicodeData.txt (40k+ entries) at startup
// and exposes a string-search index used by the GUI's per-key
// character picker. Combine with the per-key cell-assign workflow to
// let users plant arbitrary Unicode glyphs into their layouts.
package unicode

import (
	_ "embed"
	"sort"
	"strconv"
	"strings"
)

//go:embed UnicodeData.txt
var rawData string

// Char is one Unicode codepoint with its primary name and general
// category (Lu/Ll/Po/So/...). Block is a coarse human-readable
// grouping derived from the codepoint range.
type Char struct {
	Codepoint rune   `json:"cp"`
	Hex       string `json:"hex"`
	Name      string `json:"name"`
	Category  string `json:"category"`
	Block     string `json:"block"`
	Glyph     string `json:"glyph"`
}

var (
	all   []Char
	ready bool
)

// Init parses the embedded UnicodeData.txt. Idempotent — first call
// builds the index, subsequent calls are no-ops. Filter rules: skip
// control characters (Cc), surrogates (Cs), private-use, unassigned,
// and the unhelpful "<...>" placeholder names. ~30k entries survive
// after filtering — ~10MB resident, acceptable for an in-process
// search index over a long-lived server.
func Init() {
	if ready {
		return
	}
	for _, line := range strings.Split(rawData, "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, ";")
		if len(cols) < 11 {
			continue
		}
		cp, err := strconv.ParseUint(cols[0], 16, 32)
		if err != nil {
			continue
		}
		name := cols[1]
		category := cols[2]
		// Skip uninteresting categories.
		if category == "Cc" || category == "Cs" || category == "Co" || category == "Cn" {
			continue
		}
		if strings.HasPrefix(name, "<") {
			// Use the legacy name (col 10) if present, otherwise skip.
			if alt := cols[10]; alt != "" {
				name = alt
			} else {
				continue
			}
		}
		c := Char{
			Codepoint: rune(cp),
			Hex:       cols[0],
			Name:      name,
			Category:  category,
			Block:     blockFor(rune(cp)),
			Glyph:     string(rune(cp)),
		}
		all = append(all, c)
	}
	ready = true
}

// Search returns up to `limit` characters whose name or codepoint
// matches the query. Matching rules:
//   - "U+XXXX" or bare "XXXX" hex: exact codepoint lookup
//   - "X" single rune: exact codepoint lookup
//   - otherwise: case-insensitive substring on the Name field, with
//     exact-word matches ranked above substring matches
func Search(query string, limit int) []Char {
	Init()
	q := strings.TrimSpace(query)
	if q == "" {
		// Default: surface useful starting points.
		return commonStarters(limit)
	}
	// Exact codepoint forms.
	if cp, ok := parseCodepoint(q); ok {
		for _, c := range all {
			if c.Codepoint == cp {
				return []Char{c}
			}
		}
	}
	// Single-rune query: look up its codepoint directly.
	if rs := []rune(q); len(rs) == 1 {
		r := rs[0]
		for _, c := range all {
			if c.Codepoint == r {
				return []Char{c}
			}
		}
	}
	// Name substring. Rank: exact word match > word prefix > substring,
	// then by codepoint order so ties are stable.
	qup := strings.ToUpper(q)
	type scored struct {
		c    Char
		rank int
	}
	var hits []scored
	for _, c := range all {
		nm := c.Name
		idx := strings.Index(nm, qup)
		if idx < 0 {
			continue
		}
		rank := 100
		if nm == qup {
			rank = 0
		} else if isWordBoundary(nm, idx) {
			rank = 10
		}
		hits = append(hits, scored{c, rank})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		return hits[i].c.Codepoint < hits[j].c.Codepoint
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Char, len(hits))
	for i, h := range hits {
		out[i] = h.c
	}
	return out
}

// Lookup returns a single Char by codepoint, or nil if not in the
// index (e.g. a control char that was filtered out).
func Lookup(cp rune) *Char {
	Init()
	for i := range all {
		if all[i].Codepoint == cp {
			return &all[i]
		}
	}
	return nil
}

// All returns the parsed character index. Callers must treat the
// returned slice as read-only.
func All() []Char {
	Init()
	return all
}

// blockFor maps a codepoint to a coarse human-readable block. Used
// by the picker as a filter facet ("show only Greek / Currency /
// Math / Emoji…"). Falls back to "Other" for ranges we don't tag.
func blockFor(r rune) string {
	switch {
	case r >= 0x0020 && r <= 0x007E:
		return "Basic Latin"
	case r >= 0x00A0 && r <= 0x00FF:
		return "Latin-1 Supplement"
	case r >= 0x0100 && r <= 0x017F:
		return "Latin Extended-A"
	case r >= 0x0180 && r <= 0x024F:
		return "Latin Extended-B"
	case r >= 0x0370 && r <= 0x03FF:
		return "Greek"
	case r >= 0x0400 && r <= 0x04FF:
		return "Cyrillic"
	case r >= 0x0500 && r <= 0x052F:
		return "Cyrillic Supplement"
	case r >= 0x0590 && r <= 0x05FF:
		return "Hebrew"
	case r >= 0x0600 && r <= 0x06FF:
		return "Arabic"
	case r >= 0x0900 && r <= 0x097F:
		return "Devanagari"
	case r >= 0x2000 && r <= 0x206F:
		return "Punctuation"
	case r >= 0x20A0 && r <= 0x20CF:
		return "Currency"
	case r >= 0x2100 && r <= 0x214F:
		return "Letterlike"
	case r >= 0x2150 && r <= 0x218F:
		return "Number Forms"
	case r >= 0x2190 && r <= 0x21FF:
		return "Arrows"
	case r >= 0x2200 && r <= 0x22FF:
		return "Mathematical"
	case r >= 0x2300 && r <= 0x23FF:
		return "Technical"
	case r >= 0x2460 && r <= 0x24FF:
		return "Enclosed Alphanum"
	case r >= 0x2500 && r <= 0x257F:
		return "Box Drawing"
	case r >= 0x2580 && r <= 0x259F:
		return "Block Elements"
	case r >= 0x25A0 && r <= 0x25FF:
		return "Geometric"
	case r >= 0x2600 && r <= 0x26FF:
		return "Misc Symbols"
	case r >= 0x2700 && r <= 0x27BF:
		return "Dingbats"
	case r >= 0xE0000 && r <= 0xE007F:
		return "Tags"
	case r >= 0x1F300 && r <= 0x1F5FF:
		return "Symbols & Pictographs"
	case r >= 0x1F600 && r <= 0x1F64F:
		return "Emoticons"
	case r >= 0x1F680 && r <= 0x1F6FF:
		return "Transport & Map"
	case r >= 0x1F700 && r <= 0x1F77F:
		return "Alchemical"
	case r >= 0x1F780 && r <= 0x1F7FF:
		return "Geometric Ext"
	case r >= 0x1F900 && r <= 0x1F9FF:
		return "Supplemental Symbols"
	case r >= 0x1FA70 && r <= 0x1FAFF:
		return "Symbols Ext"
	}
	return "Other"
}

func parseCodepoint(s string) (rune, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "U+")
	s = strings.TrimPrefix(s, "U")
	if len(s) < 4 || len(s) > 6 {
		return 0, false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'F')) {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

func isWordBoundary(s string, idx int) bool {
	if idx == 0 {
		return true
	}
	c := s[idx-1]
	return c == ' ' || c == '-' || c == '_'
}

// commonStarters surfaces a curated default set when the user opens
// the picker with no query — popular symbols, currency, common
// punctuation, useful arrows. Pulls these by codepoint so they
// always work without depending on the parsed index.
func commonStarters(limit int) []Char {
	Init()
	wanted := []rune{
		'★', '☆', '✦', '✧', '✩', '✪', '✫', '✬', '✭', '✮', '✯', '✰', '⭐', '🌟',
		'❤', '♡', '♥', '☺', '☹', '☻',
		'☼', '☽', '☾', '☿', '♀', '♂', '☯', '☮', '✌',
		'→', '←', '↑', '↓', '↔', '⇒', '⇐', '⇑', '⇓',
		'€', '£', '¥', '¢', '₹', '₿',
		'°', '±', '×', '÷', '√', '∞', '≠', '≈', '≤', '≥', 'π', '∑', '∫', '∆',
		'©', '®', '™', '§', '¶', '†', '‡', '•', '…',
	}
	out := make([]Char, 0, len(wanted))
	for _, r := range wanted {
		if c := Lookup(r); c != nil {
			out = append(out, *c)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
