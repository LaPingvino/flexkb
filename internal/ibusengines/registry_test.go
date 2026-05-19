package ibusengines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiscoverParsesFixture writes a synthetic ibus component
// file into a temp dir, points SearchPaths at it, and verifies
// Discover finds and parses the engines correctly. No dependence
// on what's installed on the host.
func TestDiscoverParsesFixture(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.xml")
	xml := `<?xml version="1.0"?>
<component>
  <name>org.example.Test</name>
  <description>Test IM</description>
  <exec>/usr/bin/test-engine</exec>
  <version>1.0</version>
  <engines>
    <engine>
      <name>test-pinyin</name>
      <language>zh</language>
      <longname>Test Pinyin</longname>
      <description>Test Pinyin engine</description>
      <rank>50</rank>
    </engine>
    <engine>
      <name>xkb:al::sqi</name>
      <layout>al</layout>
      <longname>Albanian</longname>
    </engine>
  </engines>
</component>`
	if err := os.WriteFile(path, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := SearchPaths
	SearchPaths = []string{tmp}
	defer func() { SearchPaths = saved }()

	components, warnings := Discover()
	if len(warnings) > 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(components) != 1 {
		t.Fatalf("got %d components, want 1", len(components))
	}
	c := components[0]
	if c.Name != "org.example.Test" || c.Exec != "/usr/bin/test-engine" {
		t.Errorf("metadata: %+v", c)
	}
	if len(c.Engines) != 2 {
		t.Fatalf("got %d engines, want 2", len(c.Engines))
	}
	if c.Engines[0].LongName != "Test Pinyin" {
		t.Errorf("first engine longname: %q", c.Engines[0].LongName)
	}
}

// TestIsXKBWrapper confirms the discriminator: xkb:* names are
// layout wrappers, anything else is treated as a real IM.
func TestIsXKBWrapper(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"xkb:al::sqi", true},
		{"xkb:us::eng", true},
		{"libpinyin", false},
		{"anthy", false},
		{"hangul", false},
		{"", false}, // edge case — empty name shouldn't match prefix
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := Engine{Name: c.name}
			if got := e.IsXKBWrapper(); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestRealEnginesFilter — RealEngines drops the xkb wrappers
// from a mixed list.
func TestRealEnginesFilter(t *testing.T) {
	all := []Engine{
		{Name: "xkb:al::sqi"},
		{Name: "libpinyin"},
		{Name: "xkb:us::eng"},
		{Name: "anthy"},
	}
	real := RealEngines(all)
	if len(real) != 2 {
		t.Fatalf("got %d, want 2", len(real))
	}
	for _, e := range real {
		if strings.HasPrefix(e.Name, "xkb:") {
			t.Errorf("xkb wrapper leaked through: %s", e.Name)
		}
	}
}

// TestAllEnginesPopulatesBackrefs — AllEngines must fill in
// ComponentName + ExecPath so callers can filter by source
// without re-walking the components slice.
func TestAllEnginesPopulatesBackrefs(t *testing.T) {
	components := []Component{
		{
			Name: "org.example.A",
			Exec: "/usr/bin/a",
			Engines: []Engine{{Name: "e1"}, {Name: "e2"}},
		},
		{
			Name: "org.example.B",
			Exec: "/usr/bin/b",
			Engines: []Engine{{Name: "e3"}},
		},
	}
	flat := AllEngines(components)
	if len(flat) != 3 {
		t.Fatalf("got %d, want 3", len(flat))
	}
	for _, e := range flat {
		if e.ComponentName == "" || e.ExecPath == "" {
			t.Errorf("missing backref: %+v", e)
		}
	}
}

// TestMalformedXMLDoesNotCrashWalk — one bad file in the search
// path shouldn't hide every other file's engines.
func TestMalformedXMLDoesNotCrashWalk(t *testing.T) {
	tmp := t.TempDir()
	good := `<?xml version="1.0"?>
<component><name>good</name><exec>/x</exec>
<engines><engine><name>e</name></engine></engines>
</component>`
	bad := `<?xml version="1.0"?><not-a-component`
	os.WriteFile(filepath.Join(tmp, "a-good.xml"), []byte(good), 0o644)
	os.WriteFile(filepath.Join(tmp, "b-bad.xml"), []byte(bad), 0o644)
	saved := SearchPaths
	SearchPaths = []string{tmp}
	defer func() { SearchPaths = saved }()

	components, warnings := Discover()
	if len(components) != 1 {
		t.Errorf("got %d components, want 1 (the good one)", len(components))
	}
	if len(warnings) == 0 {
		t.Errorf("expected a warning for the malformed file")
	}
}
