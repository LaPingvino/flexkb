// Package xkbwriter serialises a ComposedLayout (or a set of them) into the
// xkb_symbols file format consumed by xkbcomp / libxkbcommon. Output is
// formatted to roughly match the hand-written /usr/share/X11/xkb/symbols/*
// files: aligned key/symbol columns, level-3-aware bracket layout.
package xkbwriter

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/lapingvino/flexkb/internal/model"
)

// Options controls cosmetic aspects of the output.
type Options struct {
	// HeaderComment is the file-level comment (without leading // markers).
	HeaderComment string
	// KeyColumnWidth pads the `key <CODE>` column. xkb files in the wild
	// use 6 (matches `key <AE01>`); allow override for unusual key codes.
	KeyColumnWidth int
}

// WriteFile writes a complete xkb_symbols file containing the supplied
// variants in order.
func WriteFile(w io.Writer, opts Options, variants []model.ComposedLayout) error {
	if opts.KeyColumnWidth == 0 {
		opts.KeyColumnWidth = 6
	}
	if opts.HeaderComment != "" {
		for _, line := range strings.Split(strings.TrimRight(opts.HeaderComment, "\n"), "\n") {
			if _, err := fmt.Fprintf(w, "// %s\n", line); err != nil {
				return err
			}
		}
		fmt.Fprintln(w)
	}
	for i, v := range variants {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if err := WriteVariant(w, opts, v); err != nil {
			return err
		}
	}
	return nil
}

// WriteVariant writes a single xkb_symbols { ... } block.
func WriteVariant(w io.Writer, opts Options, v model.ComposedLayout) error {
	header := "partial alphanumeric_keys"
	if v.Default {
		header = "default " + header
	}
	if _, err := fmt.Fprintf(w, "%s\nxkb_symbols %q {\n", header, v.Name); err != nil {
		return err
	}
	if v.Description != "" {
		if _, err := fmt.Fprintf(w, "    name[Group1]= %q;\n\n", v.Description); err != nil {
			return err
		}
	}

	// Compute symbol-column width for nice alignment within each key entry.
	maxSym := 0
	for _, k := range v.Keys {
		for _, lv := range v.Symbols[k].Levels {
			if len(lv) > maxSym {
				maxSym = len(lv)
			}
		}
	}
	if maxSym < 8 {
		maxSym = 8
	}

	// Group keys by row prefix for readability — emit rows in physical-key
	// order (preserved from the Physical layout), but insert a blank line
	// when the row-letter prefix changes (TLDE→AE, AE→AD, AD→AC, etc.).
	lastPrefix := ""
	for _, k := range v.Keys {
		sym, ok := v.Symbols[k]
		if !ok || len(sym.Levels) == 0 {
			continue
		}
		prefix := keyPrefix(k)
		if lastPrefix != "" && prefix != lastPrefix {
			fmt.Fprintln(w)
		}
		lastPrefix = prefix
		if err := writeKey(w, opts.KeyColumnWidth, maxSym, k, sym.Levels); err != nil {
			return err
		}
	}

	// Includes are emitted after a blank line, as is the xkb convention.
	if len(v.Includes) > 0 {
		fmt.Fprintln(w)
		for _, inc := range v.Includes {
			if _, err := fmt.Fprintf(w, "    include %q\n", inc); err != nil {
				return err
			}
		}
	}

	_, err := fmt.Fprintln(w, "};")
	return err
}

// keyPrefix returns the row-letter portion of an XKB key code: TLDE → TLDE,
// AE01 → AE, BKSL → BKSL, LSGT → LSGT. Used to break key listings into rows.
func keyPrefix(k string) string {
	if len(k) >= 2 && k[0] == 'A' && (k[1] == 'B' || k[1] == 'C' || k[1] == 'D' || k[1] == 'E') {
		return k[:2]
	}
	return k
}

func writeKey(w io.Writer, keyCol, symCol int, key string, levels []string) error {
	// `    key <AE01> { [ symbol1, symbol2, ... ] };`
	// Normalise: trim trailing empties, fill middle holes with NoSymbol.
	// An empty mid-array slot otherwise serialises as `, ,` which is a
	// syntax error in xkb. This can happen when locale-fill places a
	// symbol at L4 of a key that only had L1/L2 defined.
	normalised := normaliseLevels(levels)
	keyField := fmt.Sprintf("<%s>", key)
	parts := make([]string, len(normalised))
	for i, lv := range normalised {
		if i == len(normalised)-1 {
			parts[i] = lv // no padding on the last level
		} else {
			parts[i] = fmt.Sprintf("%-*s", symCol, lv)
		}
	}
	_, err := fmt.Fprintf(w, "    key %-*s {[ %s ]};\n", keyCol+2, keyField, strings.Join(parts, ", "))
	return err
}

// normaliseLevels drops trailing empty levels, replaces interior empty
// levels with `NoSymbol`, and replaces invalid tokens (anything that
// isn't a valid xkb symbol identifier) with `NoSymbol` as well. The
// xkb parser allows trailing commas in some implementations but
// rejects them in others; trimming keeps output portable. Interior
// holes and invalid tokens are illegal everywhere — a single bad
// token poisons the whole file at xkbcomp parse time.
func normaliseLevels(levels []string) []string {
	end := len(levels)
	for end > 0 && levels[end-1] == "" {
		end--
	}
	if end == 0 {
		return nil
	}
	out := make([]string, end)
	for i := 0; i < end; i++ {
		if levels[i] == "" || !isValidSymbolToken(levels[i]) {
			out[i] = "NoSymbol"
		} else {
			out[i] = levels[i]
		}
	}
	return out
}

// isValidSymbolToken reports whether s is a syntactically valid xkb
// symbol token. Real xkb symbols are bare identifiers
// (letters/digits/underscores starting with a letter or underscore),
// or hex escapes like `U017A` or `0x0100017A`. Anything else (a
// space-separated multi-token, a stray punctuation char, an empty
// string) would cause xkbcomp to abort parsing of the whole file.
func isValidSymbolToken(s string) bool {
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

// SortedVariants returns variants ordered so that the Default variant is
// first (matching xkb convention) and the rest are kept in insertion order.
func SortedVariants(variants []model.ComposedLayout) []model.ComposedLayout {
	out := append([]model.ComposedLayout(nil), variants...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return false
	})
	return out
}
