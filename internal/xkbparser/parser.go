// Package xkbparser is a minimalist parser for xkb_symbols files. It
// extracts the variant blocks, their declared name, the per-key symbol
// assignments, and any `include` directives. It deliberately does not
// implement the full xkb language — it covers the subset used in the
// `key <CODE> { [...] };` form that dominates /usr/share/X11/xkb/symbols.
//
// Comments (`// ...` and `/* ... */`) are stripped. C-style preprocessor
// constructs are not supported and not present in real xkb files.
package xkbparser

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/lapingvino/flexkb/internal/model"
)

// Variant is a parsed xkb_symbols block.
type Variant struct {
	Name        string
	Description string
	Default     bool
	// Keys preserves insertion order (the order they appeared in source).
	Keys     []string
	Symbols  map[string]model.KeySymbols
	Includes []string
}

// File is a parsed xkb symbols file.
type File struct {
	Variants []Variant
}

var (
	reLineComment  = regexp.MustCompile(`//[^\n]*`)
	reBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// Match `[default] [partial] [...keys...] xkb_symbols "name" { ... }`
	// We match the opening line up to the brace, then walk braces manually.
	reVariantHead = regexp.MustCompile(`(?ms)((?:default\s+|partial\s+|alphanumeric_keys\s+|modifier_keys\s+|keypad_keys\s+|function_keys\s+|alternate_group\s+|hidden\s+)*)xkb_symbols\s+"([^"]+)"\s*\{`)
	reNameGroup   = regexp.MustCompile(`name\s*\[\s*Group\d+\s*\]\s*=\s*"([^"]*)"\s*;`)
	reInclude     = regexp.MustCompile(`include\s+"([^"]+)"`)
	// key <CODE> {...};  -- capture code and full body
	reKey = regexp.MustCompile(`(?ms)key\s+<([A-Za-z0-9_+\-]+)>\s*\{([^}]*)\}\s*;`)
	// Extract symbol lists inside `[...]` blocks. xkb may have multiple
	// (e.g. type[...] and symbols[...]) — we collect every `[...]` and use
	// the longest, which is the symbol list.
	reBrackets = regexp.MustCompile(`(?s)\[([^\]]*)\]`)
)

// Parse parses the contents of an xkb symbols file.
func Parse(src string) (*File, error) {
	clean := reBlockComment.ReplaceAllString(src, "")
	clean = reLineComment.ReplaceAllString(clean, "")

	f := &File{}
	for {
		head := reVariantHead.FindStringIndex(clean)
		if head == nil {
			break
		}
		mods := strings.ToLower(clean[head[0]:head[1]])
		nameMatch := reVariantHead.FindStringSubmatch(clean[head[0]:head[1]])
		name := nameMatch[2]
		bodyStart := head[1]
		bodyEnd, ok := matchBraces(clean, bodyStart-1)
		if !ok {
			return nil, fmt.Errorf("unterminated xkb_symbols block %q", name)
		}
		body := clean[bodyStart : bodyEnd-1]

		v := Variant{
			Name:    name,
			Default: strings.Contains(mods, "default"),
			Symbols: map[string]model.KeySymbols{},
		}
		if m := reNameGroup.FindStringSubmatch(body); m != nil {
			v.Description = m[1]
		}
		for _, inc := range reInclude.FindAllStringSubmatch(body, -1) {
			v.Includes = append(v.Includes, inc[1])
		}
		for _, km := range reKey.FindAllStringSubmatch(body, -1) {
			code := km[1]
			block := km[2]
			levels := extractSymbols(block)
			if len(levels) == 0 {
				continue
			}
			if _, seen := v.Symbols[code]; !seen {
				v.Keys = append(v.Keys, code)
			}
			v.Symbols[code] = model.KeySymbols{Levels: levels}
		}
		f.Variants = append(f.Variants, v)

		clean = clean[bodyEnd:]
	}
	return f, nil
}

// matchBraces takes the index of an opening `{` and returns the index just
// past the matching `}`. Returns (0,false) on imbalance.
func matchBraces(s string, openIdx int) (int, bool) {
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// extractSymbols looks at the body of a `key <X> { ... }` block and returns
// the symbol list. Per xkb syntax there may be multiple `[...]` blocks (for
// type, actions, virtual modifier maps, etc.) — we pick the one with the
// most identifier-shaped commas-separated entries.
func extractSymbols(block string) []string {
	candidates := reBrackets.FindAllStringSubmatch(block, -1)
	var best []string
	for _, c := range candidates {
		parts := splitSymbolList(c[1])
		if len(parts) > len(best) {
			best = parts
		}
	}
	// Trim trailing empty strings.
	for len(best) > 0 && best[len(best)-1] == "" {
		best = best[:len(best)-1]
	}
	return best
}

func splitSymbolList(raw string) []string {
	// Symbols are comma-separated identifiers, possibly with Unicode escapes
	// like U017A or hex codes like 0x100017a. We do a simple comma split and
	// trim whitespace.
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Reject obviously-non-symbol content (type[Group1]=... metadata
		// occasionally lands inside brackets — but in well-formed xkb files
		// it's the type *outside* the brackets, so we don't normally hit it).
		if p == "" {
			out = append(out, "")
			continue
		}
		// Allow identifier-like values, hex escapes, and `NoSymbol`.
		if !validSymbolToken(p) {
			return nil
		}
		out = append(out, p)
	}
	return out
}

func validSymbolToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '+' || r == '-':
		default:
			return false
		}
	}
	return true
}
