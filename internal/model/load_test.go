package model

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLayeredLookupShadowsSystem: a higher-priority directory's same-named
// file is preferred over a lower-priority one. The lower one stays
// available for files the higher one doesn't define.
func TestLayeredLookupShadowsSystem(t *testing.T) {
	tmp := t.TempDir()
	user := filepath.Join(tmp, "user")
	sys := filepath.Join(tmp, "sys")
	for _, d := range []string{user, sys} {
		for _, s := range []string{"physical", "transformations"} {
			if err := os.MkdirAll(filepath.Join(d, s), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	// System: physical/ansi.yaml + transformations/qwerty.yaml
	writeYAML(t, filepath.Join(sys, "physical", "ansi.yaml"), "name: ANSI\nkeys: [AC01]")
	writeYAML(t, filepath.Join(sys, "transformations", "qwerty.yaml"), "name: QWERTY-system\nkeys:\n  AC01: {levels: [a, A]}")
	// User: overrides qwerty with a tweak, doesn't define a physical.
	writeYAML(t, filepath.Join(user, "transformations", "qwerty.yaml"), "name: QWERTY-user\nkeys:\n  AC01: {levels: [x, X]}")

	root := DataRoot{Paths: []string{user, sys}}

	phys, err := root.Physical("ansi")
	if err != nil {
		t.Fatal(err)
	}
	if phys.Name != "ANSI" {
		t.Errorf("system physical not found: %q", phys.Name)
	}
	trans, err := root.Transformation("qwerty")
	if err != nil {
		t.Fatal(err)
	}
	if trans.Name != "QWERTY-user" {
		t.Errorf("user transformation should shadow system: got %q", trans.Name)
	}
}

func TestLayeredLookupFallsThroughOnMiss(t *testing.T) {
	tmp := t.TempDir()
	user := filepath.Join(tmp, "user")
	sys := filepath.Join(tmp, "sys")
	if err := os.MkdirAll(filepath.Join(user, "layouts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sys, "transformations"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeYAML(t, filepath.Join(sys, "transformations", "qwerty.yaml"), "name: QWERTY\nkeys:\n  AC01: {levels: [a, A]}")
	// User dir has only a layouts/ — should not error when asked for the
	// system-provided transformation.
	root := DataRoot{Paths: []string{user, sys}}
	if _, err := root.Transformation("qwerty"); err != nil {
		t.Errorf("expected fallthrough to system, got: %v", err)
	}
}

func TestListLayoutFilesUnionsAcrossPaths(t *testing.T) {
	tmp := t.TempDir()
	user := filepath.Join(tmp, "user")
	sys := filepath.Join(tmp, "sys")
	if err := os.MkdirAll(filepath.Join(user, "layouts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sys, "layouts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeYAML(t, filepath.Join(user, "layouts", "my-mix.yaml"), "file: my-mix\nvariants: []")
	writeYAML(t, filepath.Join(sys, "layouts", "us.yaml"), "file: us\nvariants: []")
	writeYAML(t, filepath.Join(sys, "layouts", "my-mix.yaml"), "file: my-mix\nvariants: []") // duplicate name

	root := DataRoot{Paths: []string{user, sys}}
	names, err := root.ListLayoutFiles()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	if seen["us"] != 1 {
		t.Errorf("us missing")
	}
	if seen["my-mix"] != 1 {
		t.Errorf("my-mix should appear exactly once across union, got %d", seen["my-mix"])
	}
}

func writeYAML(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
