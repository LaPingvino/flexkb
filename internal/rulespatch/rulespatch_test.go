package rulespatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPatchXMLAddsNewLayout: a layout absent from upstream is appended.
func TestPatchXMLAddsNewLayout(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "in.xml")
	dst := filepath.Join(tmp, "out.xml")
	writeFile(t, src, sampleXML)

	err := PatchXML(src, dst, []LayoutInfo{
		{
			File:        "fictional",
			Description: "Fictional Land",
			Variants: []VariantInfo{
				{Name: "basic", Description: "Fictional Land (basic)"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := readFile(t, dst)
	if !strings.Contains(got, "<name>fictional</name>") {
		t.Errorf("new layout not added: %s", got)
	}
	if !strings.Contains(got, "<name>us</name>") {
		t.Errorf("existing layout removed")
	}
}

// TestPatchXMLAppendsVariantWithoutDuplicating: existing variants stay,
// new variant is appended, re-running is idempotent.
func TestPatchXMLAppendsVariantWithoutDuplicating(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "in.xml")
	dst := filepath.Join(tmp, "out.xml")
	writeFile(t, src, sampleXML)

	layouts := []LayoutInfo{
		{
			File: "us", Description: "English (US)",
			Variants: []VariantInfo{
				{Name: "my-new", Description: "English (US, custom)"},
			},
		},
	}
	if err := PatchXML(src, dst, layouts); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, dst)
	if c := strings.Count(got, "<name>my-new</name>"); c != 1 {
		t.Errorf("expected 1 my-new variant, got %d", c)
	}
	if !strings.Contains(got, "<name>dvorak</name>") {
		t.Errorf("existing dvorak variant lost: %s", got)
	}

	// Second pass — same input, same output. Idempotent.
	if err := PatchXML(dst, dst, layouts); err != nil {
		t.Fatal(err)
	}
	got2 := readFile(t, dst)
	if c := strings.Count(got2, "<name>my-new</name>"); c != 1 {
		t.Errorf("second pass duplicated: count=%d", c)
	}
}

// TestPatchLstAddsLayoutAndVariant: same guarantees in the .lst form.
func TestPatchLstAddsLayoutAndVariant(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "in.lst")
	dst := filepath.Join(tmp, "out.lst")
	writeFile(t, src, sampleLst)

	err := PatchLst(src, dst, []LayoutInfo{
		{
			File: "us", Description: "English (US)",
			Variants: []VariantInfo{
				{Name: "my-new", Description: "Custom"},
			},
		},
		{
			File: "fictional", Description: "Fictional",
			Variants: nil,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, dst)
	if !strings.Contains(got, "  fictional") {
		t.Errorf("new layout not in .lst: %s", got)
	}
	if !strings.Contains(got, "  my-new") || !strings.Contains(got, "us: Custom") {
		t.Errorf("new variant not in .lst: %s", got)
	}
	if !strings.Contains(got, "  us              ") && !strings.Contains(got, "  us ") {
		t.Errorf("existing us entry lost: %s", got)
	}
}

const sampleXML = `<?xml version="1.0"?>
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

const sampleLst = `! model
  pc105           Generic 105-key PC

! layout
  us              English (US)
  de              German

! variant
  dvorak          us: English (Dvorak)
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
