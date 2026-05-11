package bulkconvert

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyTreeFollowsSymlinkedSrcRoot is the regression test for the
// 2026-05-11 packaging bug. xkeyboard-config's meson install creates
// /usr/share/X11/xkb as a symlink to /usr/share/xkeyboard-config-2;
// when the flexkb PKGBUILD passed that symlink to `flexkb build`,
// filepath.Walk visited only the symlink node and returned without
// descending. Result: every subsequent install had only symbols/,
// no rules/compat/keycodes/types/geometry/, breaking xkbcommon for
// every GUI app at next login.
//
// Fix: CopyTree calls EvalSymlinks on srcRoot before walking. This
// test makes sure that fix stays in place — a symlinked srcRoot must
// produce the same output as the real directory.
func TestCopyTreeFollowsSymlinkedSrcRoot(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	link := filepath.Join(tmp, "linked-via-symlink")
	dst1 := filepath.Join(tmp, "dst-real")
	dst2 := filepath.Join(tmp, "dst-via-symlink")

	// Build a tree representative of xkeyboard-config: top-level subdirs
	// with assembled files, including the rules/evdev file whose absence
	// was the prod symptom.
	mustWrite := func(rel, contents string) {
		t.Helper()
		p := filepath.Join(real, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("rules/evdev", "! model = layout = variant\n")
	mustWrite("rules/evdev.lst", "! model\n  pc105 PC 105\n")
	mustWrite("compat/basic", "default xkb_compatibility \"basic\" {}\n")
	mustWrite("keycodes/evdev", "default xkb_keycodes \"evdev\" {}\n")
	mustWrite("types/basic", "default xkb_types \"basic\" {}\n")
	mustWrite("symbols/us", "xkb_symbols \"basic\" {}\n")

	// Mirror tree via real path — this is the baseline.
	if err := CopyTree(real, dst1, nil); err != nil {
		t.Fatalf("CopyTree(real): %v", err)
	}
	// And via a symlink to the same directory.
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := CopyTree(link, dst2, nil); err != nil {
		t.Fatalf("CopyTree(symlinked): %v", err)
	}

	for _, rel := range []string{
		"rules/evdev",
		"rules/evdev.lst",
		"compat/basic",
		"keycodes/evdev",
		"types/basic",
		"symbols/us",
	} {
		// Both destinations must contain every expected file.
		for _, dst := range []struct {
			name string
			path string
		}{
			{"real", dst1},
			{"symlinked", dst2},
		} {
			p := filepath.Join(dst.path, rel)
			if _, err := os.Stat(p); err != nil {
				t.Errorf("via %s source: missing %s: %v", dst.name, rel, err)
			}
		}
	}
}

// TestCopyTreeSkipFile verifies the skip map suppresses copy of named
// relative paths but other files are still copied.
func TestCopyTreeSkipFile(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "dst")
	for _, rel := range []string{"a/keep.txt", "a/skip.txt", "b/keep.txt"} {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skip := map[string]bool{"a/skip.txt": true}
	if err := CopyTree(src, dst, skip); err != nil {
		t.Fatalf("CopyTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "a/skip.txt")); err == nil {
		t.Errorf("skip.txt should NOT be copied")
	}
	for _, rel := range []string{"a/keep.txt", "b/keep.txt"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
}
