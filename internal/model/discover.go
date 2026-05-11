package model

import (
	"os"
	"path/filepath"
)

// DiscoverPaths returns an ordered list of data directories to consult:
// highest-priority first. The user can put per-user overrides in
// $XDG_CONFIG_HOME/flexkb/data (default ~/.config/flexkb/data); system
// data lives in /usr/share/flexkb/data. A repo-local ./data is preferred
// during development. Non-existent directories are pruned so callers
// don't have to special-case fresh installs.
func DiscoverPaths() []string {
	candidates := []string{}

	// 1. User overrides — XDG config home (highest priority).
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		candidates = append(candidates, filepath.Join(x, "flexkb", "data"))
	} else if home, _ := os.UserHomeDir(); home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "flexkb", "data"))
	}

	// 2. User data home — XDG data home (sometimes preferred for larger
	// authoring trees).
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		candidates = append(candidates, filepath.Join(x, "flexkb", "data"))
	} else if home, _ := os.UserHomeDir(); home != "" {
		candidates = append(candidates, filepath.Join(home, ".local", "share", "flexkb", "data"))
	}

	// 3. Repo-local ./data (dev workflow).
	if abs, err := filepath.Abs("data"); err == nil {
		candidates = append(candidates, abs)
	}

	// 4. System install (lowest priority — defaults shipped by the package).
	candidates = append(candidates, "/usr/share/flexkb/data")

	// 5. Next to the binary (xdg-honouring tarball installs).
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "data"))
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "share", "flexkb", "data"))
	}

	// Filter to directories that exist and contain *any* of the recognised
	// subdirectories. This lets a user dir that only adds new layouts (no
	// physical/transformation overrides) still participate.
	subdirs := []string{"physical", "transformations", "additions", "substitutions", "layouts"}
	out := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		for _, sd := range subdirs {
			if _, err := os.Stat(filepath.Join(abs, sd)); err == nil {
				out = append(out, abs)
				break
			}
		}
	}
	return out
}
