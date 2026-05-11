package dropincheck

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStrictSupersetIsClean: identical trees report no missing items.
func TestStrictSupersetIsClean(t *testing.T) {
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "u")
	built := filepath.Join(tmp, "b")
	makeTree(t, upstream, mockUpstream)
	makeTree(t, built, mockUpstream)

	report, err := Check(built, upstream)
	if err != nil {
		t.Fatal(err)
	}
	if !report.IsClean() {
		t.Errorf("expected clean, got %+v", report)
	}
}

// TestMissingFileIsCaught: built tree dropping a file should be flagged.
func TestMissingFileIsCaught(t *testing.T) {
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "u")
	built := filepath.Join(tmp, "b")
	makeTree(t, upstream, mockUpstream)
	missingOne := map[string]string{}
	for k, v := range mockUpstream {
		if k == "symbols/de" {
			continue
		}
		missingOne[k] = v
	}
	makeTree(t, built, missingOne)

	report, err := Check(built, upstream)
	if err != nil {
		t.Fatal(err)
	}
	if report.IsClean() {
		t.Fatalf("expected dirty report, got clean")
	}
	found := false
	for _, m := range report.MissingFiles {
		if m == "symbols/de" {
			found = true
		}
	}
	if !found {
		t.Errorf("symbols/de should be reported missing: %v", report.MissingFiles)
	}
}

// TestMissingVariantIsCaught: built file present but missing one xkb_symbols
// block is flagged.
func TestMissingVariantIsCaught(t *testing.T) {
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "u")
	built := filepath.Join(tmp, "b")
	tree := map[string]string{
		"symbols/us": twoVariants,
		"rules/evdev.xml": minimalRulesXML,
	}
	missingOne := map[string]string{
		"symbols/us": oneVariant,
		"rules/evdev.xml": minimalRulesXML,
	}
	makeTree(t, upstream, tree)
	makeTree(t, built, missingOne)

	report, err := Check(built, upstream)
	if err != nil {
		t.Fatal(err)
	}
	if report.IsClean() {
		t.Fatalf("expected dirty report")
	}
	found := false
	for _, v := range report.MissingVariants {
		if v == "us:second" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected us:second missing, got: %v", report.MissingVariants)
	}
}

// TestMissingRulesEntryIsCaught: rules registry missing a layout-variant
// pair is flagged.
func TestMissingRulesEntryIsCaught(t *testing.T) {
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "u")
	built := filepath.Join(tmp, "b")
	upstreamTree := map[string]string{
		"symbols/us": oneVariant,
		"rules/evdev.xml": rulesWithVariant,
	}
	builtTree := map[string]string{
		"symbols/us": oneVariant,
		"rules/evdev.xml": minimalRulesXML,
	}
	makeTree(t, upstream, upstreamTree)
	makeTree(t, built, builtTree)

	report, err := Check(built, upstream)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range report.MissingRules {
		if m == "us(dvorak)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected us(dvorak) missing, got: %v", report.MissingRules)
	}
}

const oneVariant = `
xkb_symbols "first" {
    key <AC01> { [ a, A ] };
};
`

const twoVariants = `
xkb_symbols "first" {
    key <AC01> { [ a, A ] };
};

xkb_symbols "second" {
    key <AC01> { [ b, B ] };
};
`

const minimalRulesXML = `<?xml version="1.0"?>
<xkbConfigRegistry version="1.1">
  <layoutList>
    <layout>
      <configItem>
        <name>us</name>
        <description>English (US)</description>
      </configItem>
      <variantList/>
    </layout>
  </layoutList>
</xkbConfigRegistry>
`

const rulesWithVariant = `<?xml version="1.0"?>
<xkbConfigRegistry version="1.1">
  <layoutList>
    <layout>
      <configItem>
        <name>us</name>
        <description>English (US)</description>
      </configItem>
      <variantList>
        <variant>
          <configItem>
            <name>dvorak</name>
            <description>English (Dvorak)</description>
          </configItem>
        </variant>
      </variantList>
    </layout>
  </layoutList>
</xkbConfigRegistry>
`

var mockUpstream = map[string]string{
	"symbols/us":      oneVariant,
	"symbols/de":      oneVariant,
	"rules/evdev.xml": minimalRulesXML,
	"types/basic":     "// types stub",
}

func makeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
