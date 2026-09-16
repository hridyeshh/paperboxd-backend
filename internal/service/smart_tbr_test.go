package service

import (
	"strings"
	"testing"
)

func TestTBRLines(t *testing.T) {
	slow := &VelocitySignal{VelocityBucket: "slow"}

	got := tbrLines(SmartTBRItem{DaysOnShelf: 430, PageCount: 600}, 300, slow)
	if len(got) != 2 || !strings.HasPrefix(got[0], "On your shelf 14 months") || !strings.Contains(got[1], "Longer") {
		t.Fatalf("stale long book: %v", got)
	}

	got = tbrLines(SmartTBRItem{DaysOnShelf: 3, PageCount: 150}, 300, nil)
	if len(got) != 2 || got[0] != "Added 3 days ago" || !strings.Contains(got[1], "Shorter") {
		t.Fatalf("fresh short book: %v", got)
	}

	// No median, no velocity: only the shelf line.
	if got = tbrLines(SmartTBRItem{DaysOnShelf: 20, PageCount: 150}, 0, nil); len(got) != 1 || got[0] != "Added 2 weeks ago" {
		t.Fatalf("no median: %v", got)
	}
	// Same day, unknown pages: nothing to say.
	if got = tbrLines(SmartTBRItem{}, 300, nil); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
}
