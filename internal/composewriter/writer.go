// Package composewriter serialises one-or-more ComposeChain entries
// into an X11 Compose file (the syntax read by libX11 and libxkbcommon
// from ~/.XCompose). The source YAML is the canonical form — this
// package is just the X-Compose adapter; a future native runtime can
// read the YAML directly without going through here.
//
// X11 Compose syntax (per `man Compose`):
//
//	<keysym1> <keysym2> ... : "result-string" [single-keysym] # comment
//
// The result-string is a UTF-8 quoted literal. The trailing single-
// keysym is optional and only meaningful when the result is exactly
// one codepoint with a named keysym; we omit it for multi-codepoint
// results, which is the dominant case for the conjuncts and triples
// this package targets.
package composewriter

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/lapingvino/flexkb/internal/model"
)

// Options controls cosmetic aspects of the output.
type Options struct {
	// HeaderComment is the file-level comment (without leading # markers).
	HeaderComment string
}

// WriteFile writes a complete X Compose file containing every sequence
// from the supplied chains, in the order given. A blank line and a
// chain-name section comment separate consecutive chains so the output
// remains readable if the user opens ~/.XCompose by hand.
func WriteFile(w io.Writer, opts Options, chains []model.ComposeChain) error {
	if opts.HeaderComment != "" {
		for _, line := range strings.Split(strings.TrimRight(opts.HeaderComment, "\n"), "\n") {
			if _, err := fmt.Fprintf(w, "# %s\n", line); err != nil {
				return err
			}
		}
		fmt.Fprintln(w)
	}
	for i, c := range chains {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if err := writeChain(w, c); err != nil {
			return err
		}
	}
	return nil
}

func writeChain(w io.Writer, c model.ComposeChain) error {
	header := c.Name
	if c.Slug != "" && c.Slug != c.Name {
		header = fmt.Sprintf("%s (%s)", c.Name, c.Slug)
	}
	if c.Description != "" {
		header = fmt.Sprintf("%s — %s", header, c.Description)
	}
	if _, err := fmt.Fprintf(w, "# %s\n", header); err != nil {
		return err
	}
	prefix := c.Prefix
	if prefix == "" {
		prefix = "Multi_key"
	}
	for _, s := range c.Sequences {
		line, err := formatSequence(prefix, s)
		if err != nil {
			return fmt.Errorf("chain %s, sequence %s: %w", c.Slug, s.ID, err)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// formatSequence renders one ComposeSequence as a single X-Compose line.
func formatSequence(prefix string, s model.ComposeSequence) (string, error) {
	if len(s.Input) == 0 {
		return "", fmt.Errorf("empty input sequence")
	}
	if len(s.Output) == 0 {
		return "", fmt.Errorf("empty output sequence")
	}
	keys := make([]string, 0, len(s.Input)+1)
	keys = append(keys, "<"+prefix+">")
	for _, k := range s.Input {
		keys = append(keys, "<"+keysymOf(k)+">")
	}
	resultStr, resultKeysym, err := renderOutput(s.Output)
	if err != nil {
		return "", err
	}
	tail := "\"" + resultStr + "\""
	if resultKeysym != "" {
		tail += " " + resultKeysym
	}
	comment := commentOf(s)
	if comment != "" {
		tail += " # " + comment
	}
	return fmt.Sprintf("%s\t: %s", strings.Join(keys, " "), tail), nil
}

// keysymOf normalises an input token. The data layer accepts both
// xkb keysym names ("a", "comma", "dead_acute") and literal characters
// in the YAML; both forms appear identically inside <…> in X Compose
// (a single ASCII letter IS its keysym), so the normalisation is a no-op
// for the common case. Non-ASCII literals would have to be authored as
// the keysym name anyway since X-Compose syntax doesn't bracket
// arbitrary Unicode.
func keysymOf(s string) string {
	return s
}

// renderOutput converts a list of Uxxxx codepoints into:
//   - the UTF-8 literal that becomes the X-Compose result string
//   - an optional trailing keysym (only filled when the result is a
//     single codepoint, which lets X Compose IM hint the named keysym
//     to the application; multi-codepoint outputs leave it empty since
//     no single keysym names a cluster).
func renderOutput(codepoints []string) (resultStr, resultKeysym string, err error) {
	var b strings.Builder
	for _, cp := range codepoints {
		r, err := decodeUEscape(cp)
		if err != nil {
			return "", "", fmt.Errorf("output token %q: %w", cp, err)
		}
		b.WriteRune(r)
	}
	resultStr = escapeQuoted(b.String())
	if len(codepoints) == 1 {
		// X Compose accepts the U-form keysym for any codepoint above
		// 0x100 as `Uxxxx`; for single-codepoint outputs we forward
		// the data form unchanged.
		resultKeysym = codepoints[0]
	}
	return resultStr, resultKeysym, nil
}

// decodeUEscape parses "U0915", "U00FC", "U10437" into the rune they
// represent. Leading "U" or "u" required; the rest is hex.
func decodeUEscape(s string) (rune, error) {
	if len(s) < 2 || (s[0] != 'U' && s[0] != 'u') {
		return 0, fmt.Errorf("not a U-escape (expected Uxxxx)")
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return 0, err
	}
	return rune(v), nil
}

// escapeQuoted escapes runes that aren't safe inside an X-Compose
// double-quoted string literal: backslash, double-quote, and ASCII
// control characters. Multi-byte UTF-8 is fine in the literal — only
// the byte-level escapes need handling.
func escapeQuoted(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%02x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func commentOf(s model.ComposeSequence) string {
	parts := []string{}
	if s.ID != "" {
		parts = append(parts, s.ID)
	}
	if s.Category != "" {
		parts = append(parts, s.Category)
	}
	if s.Description != "" {
		parts = append(parts, s.Description)
	}
	return strings.Join(parts, " — ")
}
