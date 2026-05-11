package xkbparser

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParse exercises the parser against random inputs. The contract:
// Parse must never panic on any byte string, no matter how malformed.
// It can return errors, return zero variants, or return garbage data —
// it just can't crash. Seeded with every real xkb symbols file we can
// find so the fuzzer starts from a known-valid corpus and mutates.
//
// Run with:
//   go test -fuzz=FuzzParse ./internal/xkbparser
func FuzzParse(f *testing.F) {
	// Seed corpus: a few representative xkb files (if present) so the
	// fuzzer starts close to real input.
	seedDirs := []string{
		"/usr/share/X11/xkb/symbols",
		"/usr/share/xkeyboard-config-2/symbols",
	}
	added := 0
	for _, dir := range seedDirs {
		if added >= 8 {
			break
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || added >= 8 {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			f.Add(string(b))
			added++
		}
	}
	// Pure-synthetic seeds for edge cases:
	f.Add(``)
	f.Add(`xkb_symbols "x" {};`)
	f.Add(`default partial alphanumeric_keys xkb_symbols "x" { key <AC01> { [ a, A ] }; };`)
	f.Add(`xkb_symbols "x" { /* unterminated`)
	f.Add(`xkb_symbols "y" { key <AC01> { [ a, A ] };  // commentary { ` + "\n" + ` } };`)

	f.Fuzz(func(t *testing.T, src string) {
		// The contract: never panic. Errors / empty results are fine.
		_, _ = Parse(src)
	})
}
