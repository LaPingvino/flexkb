// Package tests, file keymap_test.go: end-to-end "no variant ships empty"
// test. Builds the full xkb tree via the flexkb CLI, then walks every
// (layout, variant) pair in the patched evdev.xml and compiles it with
// xkbcomp. A variant whose compiled keymap is missing or contains no
// `key <…>` rows would appear blank in a GUI keyboard picker, so the
// test fails the build the same way a real user would notice it.
//
// Three pathologies this catches:
//
//   - Parse-time failures: a stray bareword in a symbols file (e.g. a
//     comment fragment leaking out of the bulkconvert/passthrough
//     pipeline) causes xkbcomp to abort interpretation of the whole
//     file, so every variant declared in it ships empty.
//   - Invalid token in a single key body: e.g. a multi-codepoint
//     placeholder like "U0915U094D U0937" in a substitution map.
//     Same blast radius — the file fails to parse and every variant
//     in it goes dark.
//   - Interior empty levels: `{[ p, P, , ydiaeresis ]}` emits a comma
//     pair with nothing between them, which xkbcomp also rejects.
//
// Gated on xkbcomp + FLEXKB_XKB_ROOT (or /usr/share/X11/xkb) being
// available; skipped on minimal CI.
package tests

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// emptyAllowed are evdev.xml entries that legitimately don't ship a
// symbols file — they're configuration placeholders, not real layouts.
// Anything else missing from the tree is a regression.
var emptyAllowed = map[string]bool{
	"custom/": true, // user-defined layout slot; xkeyboard-config never ships symbols/custom
}

func TestNoEmptyVariants(t *testing.T) {
	xkbcomp, err := exec.LookPath("xkbcomp")
	if err != nil {
		t.Skip("xkbcomp not installed; skipping keymap compile check")
	}
	srcXkb := xkbRoot(t) // skips test if no reference tree

	// Step 1: build the binary so we can invoke `flexkb build`.
	tmpBin := filepath.Join(t.TempDir(), "flexkb")
	repo := repoRoot(t)
	build := exec.Command("go", "build", "-o", tmpBin, "./cmd/flexkb")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build flexkb: %v\n%s", err, out)
	}

	// Step 2: build the full xkb tree.
	tree := filepath.Join(t.TempDir(), "xkb")
	gen := exec.Command(tmpBin, "build", "--xkb", srcXkb, tree)
	gen.Dir = repo
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("flexkb build: %v\n%s", err, out)
	}

	// Step 3: enumerate variants from the patched evdev.xml.
	pairs := readVariants(t, filepath.Join(tree, "rules", "evdev.xml"))
	if len(pairs) < 100 {
		t.Fatalf("only %d variants found in evdev.xml — suspiciously low", len(pairs))
	}

	// Step 4: compile each variant in parallel. xkbcomp is CPU-bound;
	// scale workers with GOMAXPROCS but cap to avoid contention.
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	jobs := make(chan [2]string, len(pairs))
	results := make(chan compileResult, len(pairs))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			workdir := filepath.Join(t.TempDir(), fmt.Sprintf("w%d", id))
			if err := os.MkdirAll(workdir, 0o755); err != nil {
				results <- compileResult{err: err.Error()}
				return
			}
			for pair := range jobs {
				layout, variant := pair[0], pair[1]
				results <- compileOne(xkbcomp, tree, workdir, layout, variant)
			}
		}(w)
	}
	for _, p := range pairs {
		jobs <- p
	}
	close(jobs)
	go func() {
		wg.Wait()
		close(results)
	}()

	var failures []string
	for r := range results {
		if r.err == "" {
			continue
		}
		key := r.layout + "/" + r.variant
		if emptyAllowed[key] {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: %s", key, r.err))
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		show := failures
		if len(show) > 25 {
			show = append(show[:25:25], fmt.Sprintf("... and %d more", len(failures)-25))
		}
		t.Fatalf("%d variant(s) compile to empty / unparseable keymaps:\n  %s",
			len(failures), strings.Join(show, "\n  "))
	}
}

type compileResult struct {
	layout, variant string
	err             string
}

// compileOne runs xkbcomp on one (layout, variant) and returns an
// error string if the result is missing or has no key bindings.
// Empty err string means success.
func compileOne(xkbcomp, tree, workdir, layout, variant string) (r compileResult) {
	r.layout = layout
	r.variant = variant
	var sym string
	if variant == "" {
		sym = fmt.Sprintf("pc+%s+inet(evdev)", layout)
	} else {
		sym = fmt.Sprintf("pc+%s(%s)+inet(evdev)", layout, variant)
	}
	// Minimum keymap that exercises the symbols include.
	stub := fmt.Sprintf(`xkb_keymap {
    xkb_keycodes  { include "evdev+aliases(qwerty)" };
    xkb_types     { include "complete" };
    xkb_compat    { include "complete" };
    xkb_symbols   { include "%s" };
    xkb_geometry  { include "pc(pc105)" };
};
`, sym)
	stubPath := filepath.Join(workdir, "stub.xkb")
	if err := os.WriteFile(stubPath, []byte(stub), 0o644); err != nil {
		r.err = "write stub: " + err.Error()
		return
	}
	outPath := filepath.Join(workdir, "out.xkb")
	_ = os.Remove(outPath)
	cmd := exec.Command(xkbcomp, "-w", "0", "-I"+tree, "-xkb", stubPath, outPath)
	stderr, _ := cmd.CombinedOutput()
	data, statErr := os.ReadFile(outPath)
	if statErr != nil {
		// xkbcomp removed the file on parse failure.
		r.err = "no output (parse failed); xkbcomp stderr extract: " + firstErrorLine(string(stderr))
		return
	}
	if !strings.Contains(string(data), "key <") {
		r.err = "compiled with zero key bindings"
		return
	}
	return
}

// firstErrorLine extracts the most useful diagnostic line from xkbcomp
// stderr — typically the first `syntax error` or `Error:` line.
func firstErrorLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "syntax error") || strings.HasPrefix(t, "Error") {
			return t
		}
	}
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}

// readVariants parses evdev.xml and returns one [layout, variant]
// pair per declared base layout AND each of its variants. Base
// (variantless) entries get variant="".
func readVariants(t *testing.T, path string) [][2]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read evdev.xml: %v", err)
	}
	type cfgItem struct {
		Name string `xml:"name"`
	}
	type variant struct {
		ConfigItem cfgItem `xml:"configItem"`
	}
	type variantList struct {
		Variants []variant `xml:"variant"`
	}
	type layout struct {
		ConfigItem  cfgItem     `xml:"configItem"`
		VariantList variantList `xml:"variantList"`
	}
	type doc struct {
		Layouts []layout `xml:"layoutList>layout"`
	}
	var d doc
	if err := xml.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse evdev.xml: %v", err)
	}
	var out [][2]string
	for _, l := range d.Layouts {
		out = append(out, [2]string{l.ConfigItem.Name, ""})
		for _, v := range l.VariantList.Variants {
			out = append(out, [2]string{l.ConfigItem.Name, v.ConfigItem.Name})
		}
	}
	return out
}

// repoRoot walks upward from the test working directory looking for a
// go.mod so we can run `go build ./cmd/flexkb` from there regardless
// of where `go test` was launched.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not locate go.mod upward from %s", wd)
	return ""
}
