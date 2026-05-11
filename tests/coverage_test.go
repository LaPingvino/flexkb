// Package tests holds integration tests that exercise flexkb against the
// real xkeyboard-config tree installed on the build host. They are gated
// on FLEXKB_XKB_ROOT being set (or defaulting to /usr/share/X11/xkb if
// present) so unit-test runs on minimal CI without xkb data still pass.
//
// The headline test is TestCoverage: it scans every symbols file in the
// reference tree, compares it against flexkb's modular layouts, and prints
// a report of how much we cover natively vs. how much falls back to a
// verbatim copy. Run with:
//
//	go test -v ./tests -run Coverage
package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/lapingvino/flexkb/internal/compose"
	"github.com/lapingvino/flexkb/internal/model"
	"github.com/lapingvino/flexkb/internal/xkbparser"
)

func xkbRoot(t *testing.T) string {
	root := os.Getenv("FLEXKB_XKB_ROOT")
	if root == "" {
		root = "/usr/share/X11/xkb"
	}
	if _, err := os.Stat(filepath.Join(root, "symbols")); err != nil {
		t.Skipf("no reference xkb tree at %s (set FLEXKB_XKB_ROOT to point elsewhere)", root)
	}
	return root
}

func dataRoot(t *testing.T) model.DataRoot {
	// Find the data directory relative to repo root, since tests run from
	// the tests/ subdirectory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "data", "physical")
		if _, err := os.Stat(candidate); err == nil {
			return model.DataRoot{Path: filepath.Join(dir, "data")}
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not locate data/ directory upward from %s", wd)
	return model.DataRoot{}
}

// TestCoverage produces a report card on how much of xkeyboard-config is
// currently covered by modular flexkb layouts. It is informational (always
// passes) unless FLEXKB_COVERAGE_MIN is set, in which case it fails if the
// per-variant exact-match ratio falls below the floor — useful for CI to
// catch regressions in data quality as we extend the modular set.
func TestCoverage(t *testing.T) {
	root := xkbRoot(t)
	data := dataRoot(t)

	symbolsDir := filepath.Join(root, "symbols")
	entries, err := os.ReadDir(symbolsDir)
	if err != nil {
		t.Fatal(err)
	}

	// Count every reference variant in the tree.
	totalVariants := 0
	totalFiles := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(symbolsDir, e.Name()))
		if err != nil {
			continue
		}
		f, err := xkbparser.Parse(string(b))
		if err != nil {
			continue
		}
		totalFiles++
		totalVariants += len(f.Variants)
	}

	// Now check our modular files.
	ourFiles, err := data.ListLayoutFiles()
	if err != nil {
		t.Fatal(err)
	}

	ourVariantCount := 0
	matched := 0
	mismatched := 0
	missingRef := 0
	keyDiffsByVariant := map[string]int{}

	for _, name := range ourFiles {
		lf, err := data.LayoutFile(name)
		if err != nil {
			t.Logf("warn loading %s: %v", name, err)
			continue
		}
		refPath := filepath.Join(symbolsDir, lf.File)
		refBytes, err := os.ReadFile(refPath)
		var refFile *xkbparser.File
		if err == nil {
			refFile, _ = xkbparser.Parse(string(refBytes))
		}

		for _, v := range lf.Variants {
			ourVariantCount++
			res, err := compose.Compose(data, v)
			if err != nil {
				t.Logf("compose %s/%s: %v", lf.File, v.Name, err)
				mismatched++
				continue
			}
			var ref *xkbparser.Variant
			if refFile != nil {
				for i := range refFile.Variants {
					if refFile.Variants[i].Name == v.Name {
						ref = &refFile.Variants[i]
						break
					}
				}
			}
			if ref == nil {
				missingRef++
				continue
			}
			diff := countKeyDiffs(res.Layout.Symbols, ref.Symbols)
			key := fmt.Sprintf("%s/%s", lf.File, v.Name)
			keyDiffsByVariant[key] = diff
			if diff == 0 {
				matched++
			} else {
				mismatched++
			}
		}
	}

	// Print the report card.
	fmt.Println()
	fmt.Println("=== flexkb coverage report ===")
	fmt.Printf("xkb tree:           %s\n", root)
	fmt.Printf("xkb symbol files:   %d\n", totalFiles)
	fmt.Printf("xkb variants total: %d\n", totalVariants)
	fmt.Println()
	fmt.Printf("flexkb modular files:    %d  (%.1f%% of xkb files)\n", len(ourFiles), pct(len(ourFiles), totalFiles))
	fmt.Printf("flexkb modular variants: %d  (%.1f%% of xkb variants)\n", ourVariantCount, pct(ourVariantCount, totalVariants))
	fmt.Println()
	fmt.Printf("  exact match:        %d  (%.1f%% of modular variants)\n", matched, pct(matched, ourVariantCount))
	fmt.Printf("  diff vs reference:  %d  (%.1f%%)\n", mismatched, pct(mismatched, ourVariantCount))
	fmt.Printf("  no reference variant in xkb (new combination): %d\n", missingRef)
	fmt.Println()
	fmt.Printf("fallback (copied verbatim from xkb): %d files (%d variants)\n",
		totalFiles-len(ourFiles), totalVariants-ourVariantCount+missingRef)
	fmt.Println()

	if len(keyDiffsByVariant) > 0 {
		fmt.Println("Per-variant key diff counts (0 = perfect match):")
		keys := make([]string, 0, len(keyDiffsByVariant))
		for k := range keyDiffsByVariant {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-30s %3d\n", k, keyDiffsByVariant[k])
		}
		fmt.Println()
	}

	if floor := os.Getenv("FLEXKB_COVERAGE_MIN"); floor != "" {
		var fmin float64
		fmt.Sscanf(floor, "%f", &fmin)
		got := pct(matched, ourVariantCount)
		if got < fmin {
			t.Fatalf("exact-match ratio %.1f%% below floor %.1f%%", got, fmin)
		}
	}
}

func countKeyDiffs(a, b map[string]model.KeySymbols) int {
	diffs := 0
	seen := map[string]bool{}
	for k, av := range a {
		seen[k] = true
		bv, ok := b[k]
		if !ok {
			diffs++
			continue
		}
		if !sameLevels(av.Levels, bv.Levels) {
			diffs++
		}
	}
	for k := range b {
		if !seen[k] {
			diffs++
		}
	}
	return diffs
}

func sameLevels(a, b []string) bool {
	a = trimNoise(a)
	b = trimNoise(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func trimNoise(a []string) []string {
	out := append([]string(nil), a...)
	for len(out) > 0 {
		last := out[len(out)-1]
		if last == "" || last == "NoSymbol" {
			out = out[:len(out)-1]
			continue
		}
		break
	}
	return out
}

// TestKnownGoodMatches asserts that the headline "we definitely cover these"
// variants stay matching. Add to this list as data improves.
func TestKnownGoodMatches(t *testing.T) {
	root := xkbRoot(t)
	data := dataRoot(t)

	known := []struct{ file, variant string }{
		{"us", "basic"},
	}

	for _, kg := range known {
		t.Run(fmt.Sprintf("%s/%s", kg.file, kg.variant), func(t *testing.T) {
			lf, err := data.LayoutFile(kg.file)
			if err != nil {
				t.Fatal(err)
			}
			var spec model.LayoutSpec
			found := false
			for _, v := range lf.Variants {
				if v.Name == kg.variant {
					spec = v
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("variant not declared in layout file")
			}
			res, err := compose.Compose(data, spec)
			if err != nil {
				t.Fatal(err)
			}
			refBytes, err := os.ReadFile(filepath.Join(root, "symbols", lf.File))
			if err != nil {
				t.Fatal(err)
			}
			refFile, err := xkbparser.Parse(string(refBytes))
			if err != nil {
				t.Fatal(err)
			}
			var ref *xkbparser.Variant
			for i := range refFile.Variants {
				if refFile.Variants[i].Name == spec.Name {
					ref = &refFile.Variants[i]
					break
				}
			}
			if ref == nil {
				t.Fatalf("reference variant not found in %s", lf.File)
			}
			diff := countKeyDiffs(res.Layout.Symbols, ref.Symbols)
			if diff != 0 {
				t.Errorf("known-good variant has %d key diffs vs reference", diff)
			}
		})
	}
	_ = strings.Builder{}
}

func pct(num, denom int) float64 {
	if denom == 0 {
		return 0
	}
	return 100 * float64(num) / float64(denom)
}
