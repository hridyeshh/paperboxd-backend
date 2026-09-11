package service

import "testing"

// The dashboard and presets are read-only views over R1–R6, so what can
// break is the mapping: an axis with no label, a preset whose constraints do
// not survive the search pipeline, a mode the button cannot serve.

func TestEveryAxisHasADashboardLabel(t *testing.T) {
	for _, axis := range TraitAxes {
		if _, ok := axisLabel[axis]; !ok {
			t.Errorf("axis %q has no dashboard label", axis)
		}
	}
}

func TestContextPresetsAreWellFormed(t *testing.T) {
	known := map[string]bool{}
	for _, a := range TraitAxes {
		known[a] = true
	}
	seen := map[string]bool{}
	for _, p := range ContextPresets() {
		if p.ID == "" || p.Label == "" || p.Hint == "" {
			t.Errorf("preset %+v missing id/label/hint", p)
		}
		if seen[p.ID] {
			t.Errorf("duplicate preset id %q", p.ID)
		}
		seen[p.ID] = true
		if len(p.Prefer) == 0 && p.MaxPages == 0 && p.MinPages == 0 {
			t.Errorf("preset %q has no constraints; it would be a plain search", p.ID)
		}
		for axis, v := range p.Prefer {
			if !known[axis] {
				t.Errorf("preset %q prefers unknown axis %q", p.ID, axis)
			}
			if v < 0 || v > 1 {
				t.Errorf("preset %q axis %q = %v out of 0..1", p.ID, axis, v)
			}
		}
	}
}

// The presets and the free-text parser must agree: typing "something for my
// flight" and tapping "I have a long flight" should pull in the same direction.
func TestContextPresetsAgreeWithParser(t *testing.T) {
	typed := ParseQuery("something for my flight tomorrow")
	var flight *ContextPreset
	for i := range contextPresets {
		if contextPresets[i].ID == "flight" {
			flight = &contextPresets[i]
		}
	}
	if flight == nil {
		t.Fatal("no flight preset")
	}
	tp, ok := typed.Constraints.PreferAxes["pacing"]
	if !ok {
		t.Fatal("typed flight query set no pacing preference")
	}
	pp := flight.Prefer["pacing"]
	if (tp > 0.5) != (pp > 0.5) {
		t.Errorf("typed pacing %v and preset pacing %v pull in opposite directions", tp, pp)
	}
}

func TestSurpriseModesAreComplete(t *testing.T) {
	want := map[string]bool{SurpriseSafe: true, SurpriseUnexpected: true, SurpriseWild: true, SurpriseGem: true, SurpriseObsession: true}
	got := SurpriseModes()
	if len(got) != len(want) {
		t.Fatalf("SurpriseModes() = %v, want %d modes", got, len(want))
	}
	for _, m := range got {
		if !want[m] {
			t.Errorf("unexpected mode %q", m)
		}
	}
}
