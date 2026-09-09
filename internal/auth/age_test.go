package auth

import (
	"testing"
	"time"
)

func TestIsAtLeastAge(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	day := func(y, m, d int) time.Time {
		return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	}

	tests := []struct {
		name string
		dob  time.Time
		want bool
	}{
		{"turns 18 tomorrow", day(2008, 9, 10), false},
		{"turns 18 today", day(2008, 9, 9), true},
		{"turned 18 yesterday", day(2008, 9, 8), true},
		{"clearly adult", day(1990, 1, 1), true},
		{"clearly a child", day(2015, 6, 1), false},
		// 2008 is a leap year; the 2026 anniversary lands on Mar 1.
		{"leap-day birthday, anniversary passed", day(2008, 2, 29), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAtLeastAge(tt.dob, now, MinimumAgeYears); got != tt.want {
				t.Fatalf("isAtLeastAge(%s) = %v, want %v", tt.dob.Format("2006-01-02"), got, tt.want)
			}
		})
	}
}

func TestValidateBirthday(t *testing.T) {
	// Empty passes on purpose: iOS and Android do not send the field yet, and
	// rejecting them would break registration from the server side.
	if err := validateBirthday(""); err != nil {
		t.Fatalf("empty birthday should pass while clients are unmigrated, got %v", err)
	}
	if err := validateBirthday("09-09-2008"); err == nil {
		t.Fatal("malformed date should be rejected")
	}
	future := time.Now().AddDate(1, 0, 0).Format("2006-01-02")
	if err := validateBirthday(future); err == nil {
		t.Fatal("future date should be rejected")
	}
	child := time.Now().AddDate(-10, 0, 0).Format("2006-01-02")
	if err := validateBirthday(child); err == nil {
		t.Fatal("under-minimum date should be rejected")
	}
	adult := time.Now().AddDate(-30, 0, 0).Format("2006-01-02")
	if err := validateBirthday(adult); err != nil {
		t.Fatalf("adult date should pass, got %v", err)
	}
}
