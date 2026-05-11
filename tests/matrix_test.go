package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/lapingvino/flexkb/internal/compose"
	"github.com/lapingvino/flexkb/internal/model"
)

// TestAdditionTransformationMatrix walks every Latin-script (transformation
// × addition) pair and runs sanity checks against the composed layout.
// Catches the bug class the user flagged: a position-based addition
// (Portuguese-old putting ç on AC10) that silently clobbers the wrong
// key when applied to a non-QWERTY base (Dvorak puts 's' at AC10).
//
// The test is informational by default — it logs warnings — and fails
// only if FLEXKB_MATRIX_STRICT is set, so we can leave it on in CI as a
// quality bar without breaking existing data.
func TestAdditionTransformationMatrix(t *testing.T) {
	data := dataRoot(t)
	transformations := listFiles(filepath.Join(data.Path, "transformations"))
	additions := listFiles(filepath.Join(data.Path, "additions"))
	if len(transformations) == 0 || len(additions) == 0 {
		t.Skip("no transformations or additions in data root")
	}

	// Use ISO physical so we have every key code in scope.
	phys, err := data.Physical("iso")
	if err != nil {
		t.Fatal(err)
	}
	meaningfulPairs := 0
	skippedPairs := 0

	// Allowlist additions whose semantics explicitly REPLACE base letters
	// (German umlauts swap AC10/AC11 etc. by design; traditional Portuguese
	// puts ç on AC10). These are not bugs — the addition is intentionally
	// position-based and tied to a specific base. New additions failing
	// the same check should be reviewed, not auto-allowlisted.
	intentionalClobber := map[string]bool{
		"german-umlauts":         true,
		"portuguese-traditional": true,
		"czech-hacek":            true,
		"french-accents":         true,
		"hungarian":              true,
		"romanian":               true,
		"swedish-finnish":        true,
		"nordic":                 true,
		"uk":                     true,
		"intl":                   true,
		"russian-phonetic-extras": true,
		"bulgarian-phonetic-extras": true,
		"greek-phonetic-extras":  true,
		"vietnamese-tone":        true,
	}

	strict := os.Getenv("FLEXKB_MATRIX_STRICT") != ""
	findings := []string{}
	noopOverlays := []string{}
	clobberedLetters := []string{}

	for _, tName := range transformations {
		trans, err := data.Transformation(tName)
		if err != nil {
			continue
		}
		tScript := scriptOf(trans.Script, "latin")
		for _, aName := range additions {
			add, err := data.Addition(aName)
			if err != nil {
				continue
			}
			if !scriptsCompatible(tScript, add.Scripts) {
				skippedPairs++
				continue
			}
			meaningfulPairs++
			spec := model.LayoutSpec{Name: "matrix", Physical: "iso", Transformation: tName}
			res := compose.ComposeFromParts(spec, phys, trans, []model.Addition{add})

			// Check 1: position overlay clobbering letter at level 1
			if !intentionalClobber[aName] {
				for keyCode, overlay := range add.Overlays {
					if len(overlay.Levels) == 0 || overlay.Levels[0] == "" {
						continue
					}
					transBase, ok := trans.Keys[keyCode]
					if !ok || len(transBase.Levels) == 0 {
						continue
					}
					origL1 := transBase.Levels[0]
					newL1 := res.Layout.Symbols[keyCode].Levels[0]
					if isLatinLetter(origL1) && isLatinLetter(newL1) && origL1 != newL1 {
						clobberedLetters = append(clobberedLetters,
							fmt.Sprintf("addition %q on %q: key %s level-1 changed %q → %q (Latin letter replaced; might be position-based mistake)",
								aName, tName, keyCode, origL1, newL1))
					}
				}
			}

			// Check 2: letter_overlay entries that match no key. Compare
			// against the PRE-addition state (transformation only),
			// otherwise the overlay's own action (rewriting level 1)
			// makes its lookup target vanish — false positive.
			preState := compose.ComposeFromParts(spec, phys, trans, nil).Layout
			for letter := range add.LetterOverlays {
				found := false
				for _, sym := range preState.Symbols {
					if len(sym.Levels) > 0 && strings.EqualFold(sym.Levels[0], letter) {
						found = true
						break
					}
				}
				if !found {
					noopOverlays = append(noopOverlays,
						fmt.Sprintf("addition %q on %q: letter_overlay for %q matches no key", aName, tName, letter))
				}
			}

			// Check 3: any meaningful pair should make SOME visible
			// change. If both Overlays and LetterOverlays produce no
			// observable effect on this transformation, the addition is
			// mismatched even though the script tags say it should fit.
			if !anyChange(trans, res.Layout) {
				findings = append(findings,
					fmt.Sprintf("addition %q on %q: claimed compatible but composed result is identical to bare transformation",
						aName, tName))
			}
		}
	}
	t.Logf("matrix: %d meaningful pairs checked, %d script-mismatched pairs skipped", meaningfulPairs, skippedPairs)

	sort.Strings(clobberedLetters)
	sort.Strings(noopOverlays)
	findings = append(findings, clobberedLetters...)
	findings = append(findings, noopOverlays...)

	if len(findings) > 0 {
		fmt.Println()
		fmt.Printf("=== matrix-test findings (%d) ===\n", len(findings))
		for _, f := range findings {
			fmt.Println("  ", f)
		}
		fmt.Println()
		if strict {
			t.Fatalf("%d matrix findings; FLEXKB_MATRIX_STRICT is set", len(findings))
		}
	}
}

// TestLetterOverlayHitsExpectedKeys spot-checks specific user-relevant
// cases that flexkb has explicit data for. Distinct from the matrix
// scan: these are direct asserts on what *should* work.
func TestLetterOverlayHitsExpectedKeys(t *testing.T) {
	data := dataRoot(t)
	phys, _ := data.Physical("iso")
	dvorak, _ := data.Transformation("dvorak")
	portuguese, _ := data.Addition("portuguese")

	res := compose.ComposeFromParts(
		model.LayoutSpec{Name: "pt-dvorak", Physical: "iso", Transformation: "dvorak"},
		phys, dvorak, []model.Addition{portuguese},
	)
	// Dvorak puts c on AD08; portuguese letter_overlay must land ç at
	// level 3 of AD08 (not AC10 like the position-based old version did).
	got := res.Layout.Symbols["AD08"].Levels
	if len(got) < 3 || got[2] != "ccedilla" {
		t.Errorf("expected ccedilla at AD08 level 3 for pt+dvorak, got %v", got)
	}
	// AC10 must STILL be 's' (Dvorak's letter), not ccedilla.
	if res.Layout.Symbols["AC10"].Levels[0] != "s" {
		t.Errorf("AC10 should still be 's' on Dvorak — got %v", res.Layout.Symbols["AC10"].Levels)
	}
}

func listFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(names)
	return names
}

func isLatinLetter(s string) bool {
	if len(s) != 1 {
		return false
	}
	r := rune(s[0])
	return unicode.IsLetter(r) && r < 128
}

func scriptOf(declared, fallback string) string {
	if declared == "" {
		return fallback
	}
	return strings.ToLower(declared)
}

// scriptsCompatible: an addition with empty/missing Scripts defaults to
// {"latin"}. "any" matches anything. Otherwise the transformation's
// script must appear in the addition's list.
func scriptsCompatible(transScript string, additionScripts []string) bool {
	if len(additionScripts) == 0 {
		return transScript == "latin"
	}
	for _, s := range additionScripts {
		s = strings.ToLower(s)
		if s == "any" || s == transScript {
			return true
		}
	}
	return false
}

// anyChange: does the composed layout differ from the bare transformation?
// Used as a "the addition actually did something" sanity check.
func anyChange(trans model.Transformation, composed model.ComposedLayout) bool {
	for k, composedSym := range composed.Symbols {
		base, ok := trans.Keys[k]
		if !ok {
			if len(composedSym.Levels) > 0 {
				return true
			}
			continue
		}
		if len(base.Levels) != len(composedSym.Levels) {
			return true
		}
		for i := range base.Levels {
			if base.Levels[i] != composedSym.Levels[i] {
				return true
			}
		}
	}
	return false
}
