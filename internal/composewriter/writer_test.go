package composewriter

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lapingvino/flexkb/internal/model"
)

func TestWriteFileEmitsValidComposeLines(t *testing.T) {
	chain := model.ComposeChain{
		Name:        "test-chain",
		Description: "test fixture",
		Prefix:      "Multi_key",
		Slug:        "test-chain",
		Sequences: []model.ComposeSequence{
			{
				ID:          "krish",
				Input:       []string{"x"},
				Output:      []string{"U0915", "U094D", "U0937"},
				Category:    "conjunct",
				Description: "क + virama + ष",
			},
			{
				ID:     "single-cp",
				Input:  []string{"o", "m"},
				Output: []string{"U0950"},
			},
		},
	}
	var buf bytes.Buffer
	if err := WriteFile(&buf, Options{HeaderComment: "test"}, []model.ComposeChain{chain}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out := buf.String()

	// Header rendered as `# ...`.
	if !strings.Contains(out, "# test") {
		t.Errorf("missing header comment; got:\n%s", out)
	}
	// Multi-codepoint krish: literal cluster, NO trailing keysym
	// (keysym is only meaningful for single-codepoint outputs).
	wantKrish := `<Multi_key> <x>` + "\t" + `: "क्ष" # krish`
	if !strings.Contains(out, wantKrish) {
		t.Errorf("krish sequence not rendered as expected; got:\n%s", out)
	}
	if strings.Contains(out, `"क्ष" U0915`) {
		t.Errorf("multi-codepoint output must not get a trailing keysym; got:\n%s", out)
	}
	// Single-codepoint om: literal char AND trailing keysym (so X
	// Compose IM can hint the named keysym to apps).
	if !strings.Contains(out, `: "ॐ" U0950`) {
		t.Errorf("single-cp sequence missing trailing keysym; got:\n%s", out)
	}
}

func TestWriteFileRejectsBadOutputToken(t *testing.T) {
	chain := model.ComposeChain{
		Name:   "bad",
		Prefix: "Multi_key",
		Sequences: []model.ComposeSequence{
			{Input: []string{"x"}, Output: []string{"not-a-u-escape"}},
		},
	}
	var buf bytes.Buffer
	err := WriteFile(&buf, Options{}, []model.ComposeChain{chain})
	if err == nil {
		t.Fatal("expected error from invalid output token")
	}
}

func TestWriteFileEscapesQuotedSpecials(t *testing.T) {
	chain := model.ComposeChain{
		Name:   "esc",
		Prefix: "Multi_key",
		Sequences: []model.ComposeSequence{
			// U005C is backslash, U0022 is double-quote — both must
			// be escaped in the result-string literal.
			{Input: []string{"slash"}, Output: []string{"U005C"}},
			{Input: []string{"quote"}, Output: []string{"U0022"}},
		},
	}
	var buf bytes.Buffer
	if err := WriteFile(&buf, Options{}, []model.ComposeChain{chain}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"\\"`) {
		t.Errorf("backslash not escaped; got:\n%s", out)
	}
	if !strings.Contains(out, `"\""`) {
		t.Errorf("double-quote not escaped; got:\n%s", out)
	}
}
