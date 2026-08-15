package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStreakOf(t *testing.T) {
	monthStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	// Aug 2 → Aug 24 read, Aug 25–26 missed, then reading resumes.
	cal := make([]int, 31)
	for i := 1; i <= 23; i++ {
		cal[i] = 50
	}
	for i := 26; i < 31; i++ {
		cal[i] = 60
	}

	s := streakOf(cal, monthStart, 31)
	if s.Days != 23 {
		t.Fatalf("days = %d, want 23", s.Days)
	}
	if s.StreakStart != 1 || s.StreakEnd != 23 {
		t.Fatalf("range = [%d,%d], want [1,23]", s.StreakStart, s.StreakEnd)
	}
	if s.Start != "Aug 2" || s.End != "Aug 24" {
		t.Fatalf("dates = %q..%q, want Aug 2..Aug 24", s.Start, s.End)
	}
	if s.BrokeIndex != 24 || s.Broke != "Aug 25" {
		t.Fatalf("broke = %d %q, want 24 Aug 25", s.BrokeIndex, s.Broke)
	}
	if s.LongestEver != 31 {
		t.Fatalf("longestEver = %d, want the stored 31", s.LongestEver)
	}

	// A month-long streak beats the stored record and replaces it.
	full := make([]int, 30)
	for i := range full {
		full[i] = 10
	}
	if got := streakOf(full, monthStart, 12).LongestEver; got != 30 {
		t.Fatalf("longestEver = %d, want 30", got)
	}

	// No reading at all: no streak, no dates, no phantom break.
	empty := streakOf(make([]int, 31), monthStart, 5)
	if empty.Days != 0 || empty.StreakStart != -1 || empty.BrokeIndex != -1 {
		t.Fatalf("empty month produced a streak: %+v", empty)
	}
}

func TestRhythmOf(t *testing.T) {
	raw := make([]int, 24)
	raw[23] = 100 // the peak
	raw[0] = 50
	raw[9] = 25

	r := rhythmOf(raw)
	if r.Label != "Night Owl" {
		t.Fatalf("label = %q, want Night Owl", r.Label)
	}
	if r.Hours[23] != 100 || r.Hours[9] != 25 {
		t.Fatalf("shape not normalised against the peak: %v", r.Hours)
	}
	// 23:00 and 00:00 are the late hours here — 150 of 175 pages.
	if r.PctAfterMidnight != 86 {
		t.Fatalf("pctAfterMidnight = %d, want 86", r.PctAfterMidnight)
	}
	if len(r.Hours) != 24 {
		t.Fatalf("hours has %d slots, want 24", len(r.Hours))
	}

	morning := make([]int, 24)
	morning[6] = 90
	if got := rhythmOf(morning).Label; got != "Early Bird" {
		t.Fatalf("label = %q, want Early Bird", got)
	}
}

func TestRankOf(t *testing.T) {
	// Beat 97 of the other 99 readers.
	r := rankOf(100, 97)
	if r.Beat != 98 || r.Percentile != 2 {
		t.Fatalf("beat/percentile = %d/%d, want 98/2", r.Beat, r.Percentile)
	}

	// Top reader never lands worse than the 1st percentile.
	if got := rankOf(1000, 999).Percentile; got != 1 {
		t.Fatalf("percentile = %d, want 1", got)
	}

	// A month with a single reader must not divide by zero.
	if got := rankOf(1, 0); got.Percentile != 100 || got.Readers != 1 {
		t.Fatalf("solo reader = %+v", got)
	}
}

func TestArchetypeOfPrefersTheStrongestSignal(t *testing.T) {
	base := WrappedResponse{
		Month:  "August",
		Totals: WrappedTotals{Books: 3, Pages: 1000, BiggestDayPages: 100, ActiveDays: 12},
		Rhythm: WrappedRhythm{Label: "Night Owl", PctAfterMidnight: 62},
	}
	if got := archetypeOf(base).Name; got != "The Midnight Romantic" {
		t.Fatalf("name = %q, want The Midnight Romantic", got)
	}

	// Same month read in daylight, but one huge day.
	day := base
	day.Rhythm = WrappedRhythm{Label: "Evening Reader", PctAfterMidnight: 4}
	day.Totals.BiggestDayPages = 400
	if got := archetypeOf(day).Name; got != "The Marathoner" {
		t.Fatalf("name = %q, want The Marathoner", got)
	}

	// Nothing distinctive still names the reader something.
	plain := day
	plain.Totals.BiggestDayPages = 90
	if got := archetypeOf(plain).Name; got == "" {
		t.Fatal("archetype has no name")
	}
}

// An empty month must still decode on the clients. iOS models the arrays as
// non-optional, so a nil slice marshalling to `null` failed the whole screen
// with "The data couldn't be read because it is missing".
func TestBlankWrappedHasNoNullArrays(t *testing.T) {
	resp := blankWrapped(
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		WrappedReader{Name: "Reader", Handle: "@reader", First: "Reader"},
	)

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// top_rated / abandoned are genuinely optional on the clients; everything
	// else must be present and non-null.
	optional := map[string]bool{"top_rated": true, "abandoned": true}
	for key, value := range decoded {
		if !optional[key] && string(value) == "null" {
			t.Errorf("%q is null — the clients cannot decode that", key)
		}
	}

	for _, path := range []string{`"traits":null`, `"hours":null`, `"calendar":null`, `"books":null`, `"authors":null`, `"genres":null`} {
		if strings.Contains(string(raw), path) {
			t.Errorf("response contains %s", path)
		}
	}

	if resp.HasData {
		t.Error("a blank month must not claim to have data")
	}
}
