package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DataRoot is one or more directories, each containing physical/,
// transformations/, additions/, substitutions/ and layouts/ subdirectories
// of YAML files. When multiple paths are configured (typical: user override
// + system install), lookups walk the Paths in order and the first match
// wins. This means a user file at $XDG_CONFIG_HOME/flexkb/data shadows the
// same-named system file, while new files in the user directory simply
// extend the available set.
//
// The single-path field Path is retained for backward compatibility with
// tests and callers that don't need layering; it's preferred when Paths is
// empty.
type DataRoot struct {
	Path  string
	Paths []string
}

// resolved returns Paths if non-empty, otherwise a single-element slice
// containing Path. Empty entries are filtered out.
func (r DataRoot) resolved() []string {
	in := r.Paths
	if len(in) == 0 {
		in = []string{r.Path}
	}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadOne tries each path in order; first existing file wins. If no path
// has the requested file, returns the error from the last attempt so the
// message names the highest-priority location.
func (r DataRoot) loadOne(subdir, name string, out any) error {
	var lastErr error
	for _, p := range r.resolved() {
		path := filepath.Join(p, subdir, name+".yaml")
		b, err := os.ReadFile(path)
		if err != nil {
			lastErr = fmt.Errorf("loading %s/%s: %w", subdir, name, err)
			continue
		}
		if err := yaml.Unmarshal(b, out); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		return nil
	}
	if lastErr == nil {
		return fmt.Errorf("no data paths configured")
	}
	return lastErr
}

func (r DataRoot) Physical(name string) (Physical, error) {
	var p Physical
	err := r.loadOne("physical", name, &p)
	p.Slug = name
	return p, err
}

func (r DataRoot) Transformation(name string) (Transformation, error) {
	var t Transformation
	err := r.loadOne("transformations", name, &t)
	t.Slug = name
	return t, err
}

func (r DataRoot) Addition(name string) (Addition, error) {
	var a Addition
	err := r.loadOne("additions", name, &a)
	a.Slug = name
	return a, err
}

// Substitution loads a substitution from data/substitutions/<name>.yaml.
// A "~" prefix on the requested name returns the inverse direction.
func (r DataRoot) Substitution(name string) (Substitution, error) {
	inverse := false
	if strings.HasPrefix(name, "~") {
		inverse = true
		name = name[1:]
	}
	var s Substitution
	if err := r.loadOne("substitutions", name, &s); err != nil {
		return s, err
	}
	s.Slug = name
	if inverse {
		return s.Inverse(), nil
	}
	return s, nil
}

func (r DataRoot) LayoutFile(name string) (LayoutFile, error) {
	var l LayoutFile
	err := r.loadOne("layouts", name, &l)
	if l.File == "" {
		l.File = name
	}
	return l, err
}

// ResolvedPaths returns the active data-search paths (in priority
// order, highest first), filtered to non-empty entries. Exposed so the
// GUI can surface user-vs-system layering directly.
func (r DataRoot) ResolvedPaths() []string { return r.resolved() }

// ReadTopLevel reads a top-level YAML file (e.g. "locales.yaml") from
// the highest-priority data path that contains it. Returns the raw
// bytes plus the source path used. Layered semantics match the
// subdir loaders: highest-priority data path wins.
func (r DataRoot) ReadTopLevel(name string) ([]byte, string, error) {
	var lastErr error
	for _, p := range r.resolved() {
		path := filepath.Join(p, name)
		b, err := os.ReadFile(path)
		if err == nil {
			return b, path, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		return nil, "", fmt.Errorf("no data paths configured")
	}
	return nil, "", lastErr
}

// Find walks the data paths in priority order and returns the absolute
// file path that would be used to load (subdir, name). Lets the GUI
// label which data layer a given module is coming from.
func (r DataRoot) Find(subdir, name string) (string, error) {
	for _, p := range r.resolved() {
		path := filepath.Join(p, subdir, name+".yaml")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s/%s.yaml not found in any data path", subdir, name)
}

// ListFillers walks every addition across the data paths and returns
// those tagged Filler: true. Used by compose to pick autofill content
// without forcing callers to also list every filler. Slug is populated
// on each result.
func (r DataRoot) ListFillers() ([]Addition, error) {
	names, err := r.ListNames("additions")
	if err != nil {
		return nil, err
	}
	out := make([]Addition, 0, len(names))
	for n := range names {
		a, err := r.Addition(n)
		if err != nil {
			continue
		}
		if !a.Filler {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// ListNames enumerates every YAML basename across all data paths in
// the given subdir. The returned map is basename -> highest-priority
// source path, matching the layered-lookup behaviour. Used by the GUI
// to populate picker dropdowns and tag each entry with its origin.
func (r DataRoot) ListNames(subdir string) (map[string]string, error) {
	out := map[string]string{}
	var lastErr error
	for _, p := range r.resolved() {
		dir := filepath.Join(p, subdir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			lastErr = err
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			n := strings.TrimSuffix(e.Name(), ".yaml")
			if _, exists := out[n]; exists {
				continue
			}
			out[n] = filepath.Join(dir, e.Name())
		}
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// ListLayoutFiles returns the basenames of all YAML files in any of the
// configured Paths' layouts/ directories. Duplicates are deduplicated;
// the union is what the user can actually compose, since lookups will
// later find the highest-priority copy.
func (r DataRoot) ListLayoutFiles() ([]string, error) {
	seen := map[string]bool{}
	var names []string
	var lastErr error
	for _, p := range r.resolved() {
		dir := filepath.Join(p, "layouts")
		entries, err := os.ReadDir(dir)
		if err != nil {
			lastErr = err
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			n := strings.TrimSuffix(e.Name(), ".yaml")
			if seen[n] {
				continue
			}
			seen[n] = true
			names = append(names, n)
		}
	}
	if len(names) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return names, nil
}
