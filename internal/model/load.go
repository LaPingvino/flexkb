package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DataRoot is a directory containing physical/, transformations/, and
// additions/ subdirectories of YAML files, plus a layouts/ directory for
// LayoutFile recipes.
type DataRoot struct {
	Path string
}

func (r DataRoot) loadOne(subdir, name string, out any) error {
	path := filepath.Join(r.Path, subdir, name+".yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("loading %s/%s: %w", subdir, name, err)
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

func (r DataRoot) Physical(name string) (Physical, error) {
	var p Physical
	err := r.loadOne("physical", name, &p)
	return p, err
}

func (r DataRoot) Transformation(name string) (Transformation, error) {
	var t Transformation
	err := r.loadOne("transformations", name, &t)
	return t, err
}

func (r DataRoot) Addition(name string) (Addition, error) {
	var a Addition
	err := r.loadOne("additions", name, &a)
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

// ListLayoutFiles returns the basenames of all YAML files in layouts/.
func (r DataRoot) ListLayoutFiles() ([]string, error) {
	dir := filepath.Join(r.Path, "layouts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	return names, nil
}
