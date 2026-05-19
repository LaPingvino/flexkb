// Package ibusengines enumerates installed ibus engine components
// by parsing the XML descriptors at /usr/share/ibus/component/.
// One step toward the "host other IMEs" goal — knowing what's
// available before we can route to them.
//
// This package is intentionally read-only. The actual subprocess
// spawning + dbus engine-side protocol implementation is a
// separate concern (see flexkb-imed/engineHost in a follow-up).
// Treating discovery as its own concern keeps the GUI's engine
// picker viable even on systems where the hosting code can't run
// (e.g. ibus-engine-libpinyin installed but flexkb's hosting
// support not yet implemented for that engine's API version).
package ibusengines

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SearchPaths is the ordered list of directories ibus-daemon
// scans for engine XML files; we use the same so installations
// in either system or user scope are discovered. Override via
// IBUS_COMPONENT_PATH in environments that put descriptors
// elsewhere.
var SearchPaths = []string{
	"/usr/share/ibus/component",
	"/usr/local/share/ibus/component",
}

// Component is one installed ibus component (a process that
// hosts one or more engines).
type Component struct {
	XMLName     xml.Name `xml:"component"`
	Name        string   `xml:"name"`
	Description string   `xml:"description"`
	Exec        string   `xml:"exec"`
	Version     string   `xml:"version"`
	Author      string   `xml:"author"`
	License     string   `xml:"license"`
	Homepage    string   `xml:"homepage"`
	TextDomain  string   `xml:"textdomain"`
	Engines     []Engine `xml:"engines>engine"`

	// SourcePath records which XML file we read this component
	// from — useful for diagnostics ("ibus-engine-libpinyin
	// installed via /usr/share/ibus/component/libpinyin.xml").
	SourcePath string `xml:"-"`
}

// Engine is one ibus engine within a Component. ibus's "engine"
// model is broad — `xkb:al::sqi` is just an Albanian xkb wrapper
// (not a real IME), while `libpinyin` is a stateful CJK IME.
// The schema is the same; we surface IsXKBWrapper to let
// callers filter.
type Engine struct {
	Name          string `xml:"name"`
	Language      string `xml:"language"`
	License       string `xml:"license"`
	Author        string `xml:"author"`
	Layout        string `xml:"layout"`
	LayoutVariant string `xml:"layout_variant"`
	LongName      string `xml:"longname"`
	Description   string `xml:"description"`
	Icon          string `xml:"icon"`
	Symbol        string `xml:"symbol"`
	Setup         string `xml:"setup"`
	Rank          int    `xml:"rank"`

	// Component back-reference for "where did this come from"
	// queries. Set by Discover after parsing.
	ComponentName string `xml:"-"`
	ExecPath      string `xml:"-"`
}

// IsXKBWrapper reports whether this engine is the "wrap an xkb
// layout in an ibus engine for the picker to expose it" pattern.
// These engines don't do any IM work — they're just layout
// surfaces. Useful to filter out when the user is looking for
// real input methods.
func (e Engine) IsXKBWrapper() bool {
	return strings.HasPrefix(e.Name, "xkb:")
}

// Discover walks the configured search paths and returns every
// component found, in stable order (sorted by source filename).
// Errors reading individual files are logged via the returned
// warnings slice but don't abort the walk — one malformed XML
// shouldn't hide every other engine.
func Discover() (components []Component, warnings []string) {
	for _, dir := range SearchPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("read %s: %v", dir, err))
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			c, err := loadComponent(path)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("parse %s: %v", path, err))
				continue
			}
			components = append(components, c)
		}
	}
	sort.SliceStable(components, func(i, j int) bool {
		return components[i].SourcePath < components[j].SourcePath
	})
	return components, warnings
}

// AllEngines is the flat-list convenience: every Engine across
// every Component, with ComponentName + ExecPath populated for
// easy filtering. Sorted by component then engine name.
func AllEngines(components []Component) []Engine {
	var out []Engine
	for _, c := range components {
		for _, e := range c.Engines {
			e.ComponentName = c.Name
			e.ExecPath = c.Exec
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ComponentName != out[j].ComponentName {
			return out[i].ComponentName < out[j].ComponentName
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// RealEngines filters out the xkb-wrapper entries to give callers
// just the "actual input methods" — Pinyin, Anthy, Mozc,
// libpinyin, etc. The xkb wrappers are valid engines per ibus's
// model but redundant with flexkb's own static layer.
func RealEngines(engines []Engine) []Engine {
	out := make([]Engine, 0, len(engines))
	for _, e := range engines {
		if !e.IsXKBWrapper() {
			out = append(out, e)
		}
	}
	return out
}

func loadComponent(path string) (Component, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Component{}, err
	}
	var c Component
	if err := xml.Unmarshal(b, &c); err != nil {
		return Component{}, fmt.Errorf("xml: %w", err)
	}
	c.SourcePath = path
	return c, nil
}
