package inputmethod

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load reads an InputMethod from one YAML file. Returns the parsed
// IM with Slug set to the file basename (sans extension).
func Load(path string) (*InputMethod, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	im, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	im.Slug = base
	return im, nil
}

// Parse reads an InputMethod from raw YAML bytes. Useful for tests
// and for callers (m17n import) that synthesise YAML in memory.
func Parse(data []byte) (*InputMethod, error) {
	var im InputMethod
	if err := yaml.Unmarshal(data, &im); err != nil {
		return nil, err
	}
	if im.Name == "" {
		return nil, fmt.Errorf("input method missing required field: name")
	}
	if len(im.States) == 0 {
		return nil, fmt.Errorf("input method %q has no states", im.Name)
	}
	if im.Trigger.Mode == "" {
		// Trigger defaults to "always" — most IMs run continuously
		// on the active text-input focus.
		im.Trigger.Mode = "always"
	}
	if im.Trigger.Mode == "prefix" && im.Trigger.PrefixKeysym == "" {
		im.Trigger.PrefixKeysym = "Multi_key"
	}
	// Validate state ID uniqueness and Goto references.
	ids := map[string]bool{}
	for _, st := range im.States {
		if st.ID == "" {
			return nil, fmt.Errorf("input method %q: state with empty id", im.Name)
		}
		if ids[st.ID] {
			return nil, fmt.Errorf("input method %q: duplicate state id %q", im.Name, st.ID)
		}
		ids[st.ID] = true
	}
	for _, st := range im.States {
		for _, tr := range st.Transitions {
			if tr.Goto != "" && !ids[tr.Goto] {
				return nil, fmt.Errorf("input method %q: state %q references unknown goto %q", im.Name, st.ID, tr.Goto)
			}
			if tr.GotoIfEmpty != "" && !ids[tr.GotoIfEmpty] {
				return nil, fmt.Errorf("input method %q: state %q references unknown goto_if_empty %q", im.Name, st.ID, tr.GotoIfEmpty)
			}
		}
	}
	return &im, nil
}
