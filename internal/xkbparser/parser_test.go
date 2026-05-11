package xkbparser

import "testing"

func TestParseBasicVariant(t *testing.T) {
	src := `
default partial alphanumeric_keys
xkb_symbols "basic" {
    name[Group1]= "English (US)";
    key <TLDE> { [ grave, asciitilde ] };
    key <AC01> { [ a, A ] };
    key <AC02> { [ s, S ] };
};
`
	f, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(f.Variants))
	}
	v := f.Variants[0]
	if v.Name != "basic" {
		t.Errorf("name: %q", v.Name)
	}
	if !v.Default {
		t.Errorf("should be default")
	}
	if v.Description != "English (US)" {
		t.Errorf("description: %q", v.Description)
	}
	if got := v.Symbols["AC01"].Levels; len(got) != 2 || got[0] != "a" || got[1] != "A" {
		t.Errorf("AC01 levels: %v", got)
	}
}

func TestParseMultipleVariants(t *testing.T) {
	src := `
xkb_symbols "first" { key <AC01> { [ a, A ] }; };
partial xkb_symbols "second" { key <AC01> { [ b, B ] }; include "level3(ralt_switch)" };
`
	f, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Variants) != 2 {
		t.Fatalf("got %d, want 2", len(f.Variants))
	}
	if f.Variants[1].Includes[0] != "level3(ralt_switch)" {
		t.Errorf("include not captured: %v", f.Variants[1].Includes)
	}
}

func TestParseStripsComments(t *testing.T) {
	src := `
// leading line comment
/* block
   comment */
xkb_symbols "x" {
    // inside
    key <AC01> { [ a, A ] }; // trailing
};
`
	f, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Variants) != 1 {
		t.Fatalf("got %d", len(f.Variants))
	}
}
