package service

import "testing"

// The taxonomy is the one thing every funnel query depends on. These cover the
// failures that would be invisible at runtime: a legacy client name silently
// becoming a fourth convention, and an anonymous row being accepted for an
// event that has no meaning without a user.

func TestNormalizeEventTypeAcceptsCanonical(t *testing.T) {
	for _, name := range EventTypes() {
		got, ok := NormalizeEventType(name)
		if !ok || got != name {
			t.Errorf("NormalizeEventType(%q) = %q, %v; want %q, true", name, got, ok, name)
		}
	}
}

func TestNormalizeEventTypeMapsLegacyNames(t *testing.T) {
	cases := map[string]string{
		"impression":          EventRecImpression,
		"book_impression":     EventRecImpression,
		"click":               EventRecClick,
		"dismiss":             EventRecDismiss,
		"book.viewed":         EventBookViewed,
		"book.added_to_shelf": EventBookAddedToShelf,
		"user.followed":       EventUserFollowed,
	}
	for in, want := range cases {
		got, ok := NormalizeEventType(in)
		if !ok {
			t.Errorf("NormalizeEventType(%q) rejected a name shipped clients still send", in)
			continue
		}
		if got != want {
			t.Errorf("NormalizeEventType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeEventTypeRejectsUnknown(t *testing.T) {
	for _, in := range []string{"", "book.opened", "randomEvent", "rec_impressions"} {
		if got, ok := NormalizeEventType(in); ok {
			t.Errorf("NormalizeEventType(%q) = %q, true; want rejection", in, got)
		}
	}
}

// Every legacy alias must resolve to a name that is itself canonical,
// otherwise the translation writes a row no query groups.
func TestLegacyAliasesResolveToCanonical(t *testing.T) {
	for alias, canonical := range legacyEventNames {
		if !ValidEventType(canonical) {
			t.Errorf("legacy alias %q maps to %q, which is not a canonical event", alias, canonical)
		}
		if ValidEventType(alias) {
			t.Errorf("%q is both an alias and canonical; the alias is dead code", alias)
		}
	}
}

// Anonymous rows are only meaningful for pre-signup behaviour. A shelf or
// rating event with no user is a corrupt funnel row, not a privacy feature.
func TestAnonymousEventsAreASubsetAndPreSignupOnly(t *testing.T) {
	forbidden := map[string]bool{
		EventBookFinished: true, EventBookRated: true, EventBookAddedToShelf: true,
		EventRecLoved: true, EventRecNotForMe: true, EventUserFollowed: true,
		EventThoughtCreated: true, EventOnboardingCompleted: true,
	}
	for name := range anonymousEventTypes {
		if !ValidEventType(name) {
			t.Errorf("%q allows anonymous writes but is not a canonical event", name)
		}
		if forbidden[name] {
			t.Errorf("%q must never be recorded without a user", name)
		}
		if !AllowsAnonymous(name) {
			t.Errorf("AllowsAnonymous(%q) = false for a member of the set", name)
		}
	}
	if AllowsAnonymous(EventBookFinished) {
		t.Error("AllowsAnonymous(book_finished) = true; want false")
	}
}
