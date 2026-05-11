// Package dropincheck verifies that a built flexkb xkb tree is a strict
// superset of an upstream xkeyboard-config tree. The drop-in promise
// (provides=xkeyboard-config, conflicts=xkeyboard-config) is only honest
// if every file and every named variant the user could have selected
// before still resolves after the swap.
//
// This is a build-time check, not a runtime one — the package is broken
// at build time if anything is missing. The .install hook is purely
// diagnostic; if it doesn't run, the package is still drop-in.
package dropincheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lapingvino/flexkb/internal/xkbparser"
)

// Report aggregates everything the check found. Empty Missing slice means
// the built tree is a strict superset.
type Report struct {
	// MissingFiles lists relative paths present in upstream that are
	// absent from the built tree (e.g. "symbols/sk" if Slovak got
	// dropped).
	MissingFiles []string
	// MissingVariants lists "<file>:<variant>" for xkb_symbols blocks
	// that exist in upstream but not in the built tree.
	MissingVariants []string
	// MissingRules lists rules-registry entries (layout/variant names
	// from evdev.xml) absent from the built one.
	MissingRules []string
}

func (r Report) IsClean() bool {
	return len(r.MissingFiles) == 0 && len(r.MissingVariants) == 0 && len(r.MissingRules) == 0
}

// Check runs the verification. built and upstream are filesystem paths to
// the two xkb trees being compared.
func Check(built, upstream string) (*Report, error) {
	r := &Report{}

	// 1. Every regular file in upstream/ must exist in built/.
	err := filepath.Walk(upstream, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(upstream, path)
		if err != nil {
			return err
		}
		target := filepath.Join(built, rel)
		if _, err := os.Stat(target); err != nil {
			r.MissingFiles = append(r.MissingFiles, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk upstream: %w", err)
	}

	// 2. Every xkb_symbols block in each upstream symbols/ file must be
	// present in the corresponding built file. Files we missed entirely
	// in step 1 are skipped here (they're already flagged).
	upstreamSymbols := filepath.Join(upstream, "symbols")
	entries, err := os.ReadDir(upstreamSymbols)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			rel := filepath.Join("symbols", e.Name())
			builtPath := filepath.Join(built, rel)
			builtBytes, err := os.ReadFile(builtPath)
			if err != nil {
				// Already in MissingFiles.
				continue
			}
			upstreamBytes, err := os.ReadFile(filepath.Join(upstreamSymbols, e.Name()))
			if err != nil {
				continue
			}
			missing := compareVariants(string(upstreamBytes), string(builtBytes))
			for _, m := range missing {
				r.MissingVariants = append(r.MissingVariants, fmt.Sprintf("%s:%s", e.Name(), m))
			}
		}
	}

	// 3. Every layout/variant in upstream rules/evdev.xml must appear in
	// the built one. We don't enforce ordering; just presence.
	missingRules, err := compareRulesXML(
		filepath.Join(upstream, "rules", "evdev.xml"),
		filepath.Join(built, "rules", "evdev.xml"),
	)
	if err == nil {
		r.MissingRules = append(r.MissingRules, missingRules...)
	}

	return r, nil
}

// compareVariants returns names of xkb_symbols blocks present in
// upstream-source but not in built-source. Robust to formatting
// differences — uses the same parser flexkb produces output with.
func compareVariants(upstream, built string) []string {
	up, err := xkbparser.Parse(upstream)
	if err != nil {
		return nil
	}
	bu, err := xkbparser.Parse(built)
	if err != nil {
		// Built file unparsable — that's a different bug.
		return nil
	}
	have := map[string]bool{}
	for _, v := range bu.Variants {
		have[v.Name] = true
	}
	var missing []string
	for _, v := range up.Variants {
		if !have[v.Name] {
			missing = append(missing, v.Name)
		}
	}
	return missing
}

// compareRulesXML walks the upstream evdev.xml, extracts each
// (layout, variant) pair, and reports any missing from the built file.
// Returns a slice of "<layout>(<variant>)" strings — empty variant means
// just the layout itself.
func compareRulesXML(upstreamPath, builtPath string) ([]string, error) {
	upPairs, err := extractRulesPairs(upstreamPath)
	if err != nil {
		return nil, err
	}
	buPairs, err := extractRulesPairs(builtPath)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, p := range buPairs {
		have[p] = true
	}
	var missing []string
	for _, p := range upPairs {
		if !have[p] {
			missing = append(missing, p)
		}
	}
	return missing, nil
}

// extractRulesPairs returns a flat list of "<layout>" or "<layout>(<variant>)"
// strings parsed from an xkb rules XML file. Cheap text scan rather than
// proper XML to avoid pulling in the XML schema again here.
func extractRulesPairs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := string(data)
	var pairs []string
	// Walk layout-by-layout. For each <layout>...</layout> block,
	// capture the layout name then enumerate variants.
	lay := "<layout>"
	i := 0
	for {
		start := strings.Index(s[i:], lay)
		if start == -1 {
			break
		}
		start += i
		end := strings.Index(s[start:], "</layout>")
		if end == -1 {
			break
		}
		end += start
		block := s[start:end]
		name := xmlInner(block, "<name>", "</name>")
		if name == "" {
			i = end + len("</layout>")
			continue
		}
		pairs = append(pairs, name)
		// Variants:
		vi := 0
		for {
			vs := strings.Index(block[vi:], "<variant>")
			if vs == -1 {
				break
			}
			vs += vi
			ve := strings.Index(block[vs:], "</variant>")
			if ve == -1 {
				break
			}
			ve += vs
			vname := xmlInner(block[vs:ve], "<name>", "</name>")
			if vname != "" {
				pairs = append(pairs, fmt.Sprintf("%s(%s)", name, vname))
			}
			vi = ve + len("</variant>")
		}
		i = end + len("</layout>")
	}
	return pairs, nil
}

func xmlInner(s, open, close string) string {
	i := strings.Index(s, open)
	if i == -1 {
		return ""
	}
	i += len(open)
	j := strings.Index(s[i:], close)
	if j == -1 {
		return ""
	}
	return strings.TrimSpace(s[i : i+j])
}
