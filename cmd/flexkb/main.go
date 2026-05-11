// Command flexkb is the CLI for the flexkb modular xkb generator.
//
// Subcommands:
//
//	compose <layout-file> <variant>      print one variant to stdout
//	generate <out-dir>                   write all layouts to <out-dir>/symbols/
//	build <out-dir> [--xkb=/path]        generate modular + fallback-copy rest
//	verify <layout-file> <variant>       compose then compare against system xkb
//	list                                 list known modular layouts
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lapingvino/flexkb/internal/bulkconvert"
	"github.com/lapingvino/flexkb/internal/compose"
	"github.com/lapingvino/flexkb/internal/model"
	"github.com/lapingvino/flexkb/internal/xkbparser"
	"github.com/lapingvino/flexkb/internal/xkbwriter"
)

const defaultXKBRoot = "/usr/share/X11/xkb"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "compose":
		runCompose(args)
	case "generate":
		runGenerate(args)
	case "build":
		runBuild(args)
	case "verify":
		runVerify(args)
	case "list":
		runList(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `flexkb — modular xkb layout generator

Usage:
  flexkb compose [--data DIR] <layout-file> <variant>
  flexkb generate [--data DIR] <output-dir>
  flexkb build [--data DIR] [--xkb /usr/share/X11/xkb] <output-dir>
  flexkb verify [--data DIR] [--xkb /usr/share/X11/xkb] <layout-file> <variant>
  flexkb list [--data DIR]

Commands:
  compose   Emit one variant to stdout (debug / preview).
  generate  Write all modular layouts to <output-dir>/symbols/.
  build     generate, then copy any remaining xkb tree from --xkb verbatim,
            so output is a complete drop-in /usr/share/X11/xkb replacement.
  verify    Compose a variant and compare key-by-key against the same-named
            variant in <xkb>/symbols/<layout-file>. Useful for round-trip
            testing the modular decomposition.
  list      Print the layout-file / variant matrix flexkb knows about.`)
}

// dataDir pulls --data / --data=… out of args (leaving everything else,
// including --xkb, untouched for the subcommand-specific parser to handle).
func dataDir(args []string) (string, []string) {
	out := findDefaultDataDir()
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--data" && i+1 < len(args):
			out = args[i+1]
			i++
		case strings.HasPrefix(a, "--data="):
			out = strings.TrimPrefix(a, "--data=")
		default:
			rest = append(rest, a)
		}
	}
	return out, rest
}

// findDefaultDataDir walks upward from the binary location and the cwd
// looking for a `data/physical` directory — so `flexkb` works both from
// the repo root during development and from /usr/share/flexkb after install.
func findDefaultDataDir() string {
	candidates := []string{
		"data",
		"/usr/share/flexkb/data",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates,
			filepath.Join(filepath.Dir(exe), "data"),
			filepath.Join(filepath.Dir(exe), "..", "share", "flexkb", "data"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "physical")); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return "data"
}

// xkbDir extracts --xkb from args; used by build/verify which both need it.
func xkbDir(args []string) (string, []string) {
	out := defaultXKBRoot
	rest := args[:0]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--xkb" && i+1 < len(args):
			out = args[i+1]
			i++
		case strings.HasPrefix(a, "--xkb="):
			out = strings.TrimPrefix(a, "--xkb=")
		default:
			rest = append(rest, a)
		}
	}
	return out, rest
}

func runCompose(args []string) {
	data, rest := dataDir(args)
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: flexkb compose <layout-file> <variant>")
		os.Exit(2)
	}
	root := model.DataRoot{Path: data}
	lf, err := root.LayoutFile(rest[0])
	check(err)
	for _, v := range lf.Variants {
		if v.Name != rest[1] {
			continue
		}
		res, err := compose.Compose(root, v)
		check(err)
		for _, w := range res.Warnings {
			fmt.Fprintln(os.Stderr, "warn:", w)
		}
		check(xkbwriter.WriteVariant(os.Stdout, xkbwriter.Options{}, res.Layout))
		return
	}
	fmt.Fprintf(os.Stderr, "variant %q not found in %s\n", rest[1], rest[0])
	os.Exit(1)
}

func runGenerate(args []string) {
	data, rest := dataDir(args)
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: flexkb generate <output-dir>")
		os.Exit(2)
	}
	out := rest[0]
	root := model.DataRoot{Path: data}
	names, err := root.ListLayoutFiles()
	check(err)
	symbolsDir := filepath.Join(out, "symbols")
	check(os.MkdirAll(symbolsDir, 0o755))
	for _, name := range names {
		if err := generateOne(root, name, symbolsDir); err != nil {
			fmt.Fprintf(os.Stderr, "generating %s: %v\n", name, err)
			os.Exit(1)
		}
	}
	fmt.Printf("wrote %d layout file(s) to %s\n", len(names), symbolsDir)
}

func generateOne(root model.DataRoot, name, symbolsDir string) error {
	lf, err := root.LayoutFile(name)
	if err != nil {
		return err
	}
	var composed []model.ComposedLayout
	for _, v := range lf.Variants {
		res, err := compose.Compose(root, v)
		if err != nil {
			return err
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(os.Stderr, "warn (%s/%s): %s\n", name, v.Name, w)
		}
		composed = append(composed, res.Layout)
	}
	f, err := os.Create(filepath.Join(symbolsDir, lf.File))
	if err != nil {
		return err
	}
	defer f.Close()
	opts := xkbwriter.Options{HeaderComment: fmt.Sprintf("Generated by flexkb for %q. Edit data/ sources, not this file.", lf.File)}
	return xkbwriter.WriteFile(f, opts, composed)
}

func runBuild(args []string) {
	data, rest := dataDir(args)
	xkb, rest := xkbDir(rest)
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: flexkb build [--xkb /path] <output-dir>")
		os.Exit(2)
	}
	out := rest[0]
	root := model.DataRoot{Path: data}

	// Step 1: list which symbols files we own modularly — they'll be skipped
	// by the fallback copy in step 3.
	names, err := root.ListLayoutFiles()
	check(err)
	skip := map[string]bool{}
	for _, n := range names {
		lf, err := root.LayoutFile(n)
		check(err)
		skip[filepath.Join("symbols", lf.File)] = true
	}

	// Step 2: copy the rest of the upstream xkb tree.
	check(bulkconvert.CopyTree(xkb, out, skip))

	// Step 3: write our modular symbols files on top.
	symbolsDir := filepath.Join(out, "symbols")
	check(os.MkdirAll(symbolsDir, 0o755))
	for _, n := range names {
		check(generateOne(root, n, symbolsDir))
	}
	fmt.Printf("built complete xkb tree in %s (%d modular layout file(s), rest copied from %s)\n", out, len(names), xkb)
}

func runVerify(args []string) {
	data, rest := dataDir(args)
	xkb, rest := xkbDir(rest)
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: flexkb verify [--xkb /path] <layout-file> <variant>")
		os.Exit(2)
	}
	root := model.DataRoot{Path: data}
	lf, err := root.LayoutFile(rest[0])
	check(err)

	var spec model.LayoutSpec
	found := false
	for _, v := range lf.Variants {
		if v.Name == rest[1] {
			spec = v
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "variant %q not found in layout %q\n", rest[1], rest[0])
		os.Exit(1)
	}

	res, err := compose.Compose(root, spec)
	check(err)
	composed := res.Layout

	// Read the reference xkb file.
	refPath := filepath.Join(xkb, "symbols", lf.File)
	refBytes, err := os.ReadFile(refPath)
	check(err)
	parsed, err := xkbparser.Parse(string(refBytes))
	check(err)

	var ref *xkbparser.Variant
	for i := range parsed.Variants {
		if parsed.Variants[i].Name == spec.Name {
			ref = &parsed.Variants[i]
			break
		}
	}
	if ref == nil {
		fmt.Fprintf(os.Stderr, "reference variant %q not found in %s\n", spec.Name, refPath)
		os.Exit(1)
	}

	diffs := compareKeys(composed.Symbols, ref.Symbols)
	if len(diffs) == 0 {
		fmt.Printf("OK: composed %s/%s matches %s\n", lf.File, spec.Name, refPath)
		return
	}
	sort.Strings(diffs)
	fmt.Printf("DIFF: composed %s/%s differs from %s in %d key(s):\n", lf.File, spec.Name, refPath, len(diffs))
	for _, d := range diffs {
		fmt.Println("  ", d)
	}
	os.Exit(1)
}

func compareKeys(a, b map[string]model.KeySymbols) []string {
	var diffs []string
	seen := map[string]bool{}
	for k, av := range a {
		seen[k] = true
		bv, ok := b[k]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: composed=%v reference=<absent>", k, av.Levels))
			continue
		}
		if !sameLevels(av.Levels, bv.Levels) {
			diffs = append(diffs, fmt.Sprintf("%s: composed=%v reference=%v", k, av.Levels, bv.Levels))
		}
	}
	for k, bv := range b {
		if seen[k] {
			continue
		}
		diffs = append(diffs, fmt.Sprintf("%s: composed=<absent> reference=%v", k, bv.Levels))
	}
	return diffs
}

func sameLevels(a, b []string) bool {
	// Reference (xkb) often pads to 4 levels with duplicates of level 1 or
	// asciitilde for "do nothing" — we treat trailing-empty / trailing-NoSymbol
	// as equivalent to absence so the comparison isn't drowned in noise.
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

func runList(args []string) {
	data, _ := dataDir(args)
	root := model.DataRoot{Path: data}
	names, err := root.ListLayoutFiles()
	check(err)
	for _, n := range names {
		lf, err := root.LayoutFile(n)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}
		for _, v := range lf.Variants {
			adds := strings.Join(v.Additions, "+")
			if adds == "" {
				adds = "-"
			}
			subs := strings.Join(v.Substitutions, "+")
			if subs == "" {
				subs = "-"
			}
			fmt.Printf("%s\t%s\t%s + %s + add:%s + sub:%s\n", lf.File, v.Name, v.Physical, v.Transformation, adds, subs)
		}
	}
}

func check(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
