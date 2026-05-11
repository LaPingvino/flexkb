package compose

import (
	"testing"

	"github.com/lapingvino/flexkb/internal/model"
)

func TestComposeAppliesTransformationThenAdditions(t *testing.T) {
	phys := model.Physical{Name: "tiny", Keys: []string{"AC01", "AC02"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A"}},
		"AC02": {Levels: []string{"s", "S"}},
	}}
	add := model.Addition{Name: "ogonek", Overlays: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"", "", "aogonek", "Aogonek"}},
	}}
	spec := model.LayoutSpec{Name: "test", Physical: "tiny", Transformation: "qw"}
	r := ComposeFromParts(spec, phys, trans, []model.Addition{add})
	got := r.Layout.Symbols["AC01"].Levels
	want := []string{"a", "A", "aogonek", "Aogonek"}
	if len(got) != len(want) {
		t.Fatalf("AC01 levels: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AC01[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
	// AC02 untouched.
	gotS := r.Layout.Symbols["AC02"].Levels
	if len(gotS) != 2 || gotS[0] != "s" || gotS[1] != "S" {
		t.Errorf("AC02 should be unchanged, got %v", gotS)
	}
}

func TestComposeDropsKeysNotOnPhysical(t *testing.T) {
	phys := model.Physical{Name: "ansi-tiny", Keys: []string{"AC01"}}
	trans := model.Transformation{Name: "qw", Keys: map[string]model.KeySymbols{
		"AC01": {Levels: []string{"a", "A"}},
		"LSGT": {Levels: []string{"backslash", "bar"}},
	}}
	r := ComposeFromParts(model.LayoutSpec{Name: "t"}, phys, trans, nil)
	if _, ok := r.Layout.Symbols["LSGT"]; ok {
		t.Errorf("LSGT should be dropped on ANSI physical")
	}
	if len(r.Warnings) == 0 {
		t.Errorf("expected a warning for dropped LSGT")
	}
}

func TestAdditionPassThroughEmptyLevels(t *testing.T) {
	got := mergeLevels([]string{"a", "A"}, []string{"", "", "aogonek", "Aogonek"})
	want := []string{"a", "A", "aogonek", "Aogonek"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestAdditionTrimsTrailingEmpties(t *testing.T) {
	got := mergeLevels([]string{"a", "A", "", ""}, []string{"a", "A"})
	if len(got) != 2 {
		t.Errorf("trailing empties should be trimmed: got %v", got)
	}
}
