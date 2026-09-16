package service

// Canonical analytics event names.
//
// Every client and handler writes through this list. Before it existed the
// table held three conventions at once -- "book.viewed" from the server,
// "impression" from recommendation feedback, "book_impression" from iOS -- so
// grouping by event_type split one behaviour across several rows and no funnel
// could be computed. 000042 renamed the history; ValidEventType keeps new
// writes from reintroducing the problem.
//
// The list is enforced in Go rather than by a CHECK constraint so that adding
// an event is a code change, not a migration.
const (
	// Acquisition. These are the events that may arrive with no user_id, from
	// a visitor who has not signed up yet.
	EventLandingViewed   = "landing_viewed"
	EventSignupStarted   = "signup_started"
	EventSignupCompleted = "signup_completed"

	// Onboarding.
	EventOnboardingStarted        = "onboarding_started"
	EventOnboardingStep           = "onboarding_step"
	EventOnboardingBookSelected   = "onboarding_book_selected"
	EventOnboardingReaderFollowed = "onboarding_reader_followed"
	EventOnboardingCompleted      = "onboarding_completed"

	// Book lifecycle -- the spine of the reading funnel.
	EventBookViewed            = "book_viewed"
	EventBookAddedToShelf      = "book_added_to_shelf"
	EventBookRemovedFromShelf  = "book_removed_from_shelf"
	EventBookStarted           = "book_started"
	EventBookFinished          = "book_finished"
	EventBookRated             = "book_rated"
	EventBookLiked             = "book_liked"
	EventBookUnliked           = "book_unliked"
	EventBookSearched          = "book_searched"
	EventReadingProgressUpdate = "reading_progress_updated"

	// Discovery surfaces.
	EventFeedViewed      = "feed_viewed"
	EventSearchPerformed = "search_performed"
	EventVibeSearch      = "vibe_search_performed"
	EventProfileViewed   = "profile_viewed"

	// Surface-specific nudges the web home renders.
	EventTbrNudgeClicked         = "tbr_nudge_clicked"
	EventSuggestedReaderFollowed = "suggested_reader_followed"

	// Paperboxd Daily. metadata.kind and metadata.slug say which atom.
	EventDailyOpened = "daily_opened"
	EventDailyShared = "daily_shared"

	// Recommendation feedback. rec_impression/click/dismiss are the three the
	// clients already sent under other names; the rest arrive with R2.
	EventRecImpression  = "rec_impression"
	EventRecClick       = "rec_click"
	EventRecDismiss     = "rec_dismiss"
	EventRecLoved       = "rec_loved"
	EventRecMaybe       = "rec_maybe"
	EventRecNotForMe    = "rec_not_for_me"
	EventRecAlreadyRead = "rec_already_read"
	EventRecNotNow      = "rec_not_now"

	// Social.
	EventUserFollowed   = "user_followed"
	EventUserUnfollowed = "user_unfollowed"

	// Thoughts and lists.
	EventThoughtCreated  = "thought_created"
	EventThoughtLiked    = "thought_liked"
	EventThoughtReposted = "thought_reposted"
	EventListCreated     = "list_created"
	EventListBookAdded   = "list_book_added"
	EventListShared      = "list_shared"

	// Plus. The client sends the first two with metadata.feature and
	// metadata.surface; the subscription ones come from the link endpoints and
	// store webhooks, so they reflect what the store said, not what the app
	// hoped.
	EventPaywallViewed         = "paywall_viewed"
	EventPremiumCTAClicked     = "premium_cta_clicked"
	EventPremiumFeatureViewed  = "premium_feature_viewed"
	EventPremiumFeatureUsed    = "premium_feature_used"
	EventSubscriptionStarted   = "subscription_started"
	EventTrialStarted          = "trial_started"
	EventSubscriptionRenewed   = "subscription_renewed"
	EventSubscriptionCancelled = "subscription_cancelled"
	EventSubscriptionExpired   = "subscription_expired"
)

// validEventTypes is the set ValidEventType checks against.
var validEventTypes = map[string]struct{}{
	EventLandingViewed: {}, EventSignupStarted: {}, EventSignupCompleted: {},

	EventOnboardingStarted: {}, EventOnboardingStep: {}, EventOnboardingCompleted: {},
	EventOnboardingBookSelected: {}, EventOnboardingReaderFollowed: {},

	EventBookViewed: {}, EventBookAddedToShelf: {}, EventBookRemovedFromShelf: {},
	EventBookStarted: {}, EventBookFinished: {}, EventBookRated: {},
	EventBookLiked: {}, EventBookUnliked: {}, EventBookSearched: {},
	EventReadingProgressUpdate: {},

	EventFeedViewed: {}, EventSearchPerformed: {}, EventVibeSearch: {},
	EventProfileViewed: {}, EventTbrNudgeClicked: {}, EventSuggestedReaderFollowed: {},
	EventDailyOpened: {}, EventDailyShared: {},

	EventRecImpression: {}, EventRecClick: {}, EventRecDismiss: {},
	EventRecLoved: {}, EventRecMaybe: {}, EventRecNotForMe: {},
	EventRecAlreadyRead: {}, EventRecNotNow: {},

	EventUserFollowed: {}, EventUserUnfollowed: {},

	EventThoughtCreated: {}, EventThoughtLiked: {}, EventThoughtReposted: {},
	EventListCreated: {}, EventListBookAdded: {}, EventListShared: {},

	EventPaywallViewed: {}, EventPremiumCTAClicked: {}, EventPremiumFeatureViewed: {},
	EventPremiumFeatureUsed: {}, EventSubscriptionStarted: {}, EventTrialStarted: {},
	EventSubscriptionRenewed: {}, EventSubscriptionCancelled: {}, EventSubscriptionExpired: {},
}

// anonymousEventTypes may be written without a user_id. Everything else
// requires an authenticated actor: an anonymous "book_finished" is not a
// privacy win, it is a corrupt funnel.
var anonymousEventTypes = map[string]struct{}{
	EventLandingViewed:   {},
	EventSignupStarted:   {},
	EventBookViewed:      {},
	EventSearchPerformed: {},
	EventVibeSearch:      {},
}

// legacyEventNames maps the names already in the wild to canonical ones.
//
// Shipped iOS and Android builds cannot be changed retroactively -- they will
// keep sending "book_impression" and the bare recommendation verbs for as long
// as those versions are installed. Translating at the API boundary is what lets
// 000042 normalise the history without orphaning those clients.
var legacyEventNames = map[string]string{
	"impression":               EventRecImpression,
	"book_impression":          EventRecImpression,
	"click":                    EventRecClick,
	"dismiss":                  EventRecDismiss,
	"book.viewed":              EventBookViewed,
	"book.added_to_shelf":      EventBookAddedToShelf,
	"book.removed_from_shelf":  EventBookRemovedFromShelf,
	"book.finished":            EventBookFinished,
	"book.liked":               EventBookLiked,
	"book.unliked":             EventBookUnliked,
	"book.searched":            EventBookSearched,
	"diary.entry_created":      EventThoughtCreated,
	"diary_entry_created":      EventThoughtCreated,
	"diary.entry_liked":        EventThoughtLiked,
	"diary_entry_liked":        EventThoughtLiked,
	"list.created":             EventListCreated,
	"list.book_added":          EventListBookAdded,
	"list.shared":              EventListShared,
	"user.followed":            EventUserFollowed,
	"user.unfollowed":          EventUserUnfollowed,
	"reading.progress_updated": EventReadingProgressUpdate,

	// Names the web client shipped before the taxonomy was settled. Kept as
	// aliases so a client deploy and a backend deploy do not have to land in
	// the same minute.
	"home_viewed":                       EventFeedViewed,
	"reading_finished":                  EventBookFinished,
	"tbr_added":                         EventBookAddedToShelf,
	"book_selected_during_onboarding":   EventOnboardingBookSelected,
	"person_followed_during_onboarding": EventOnboardingReaderFollowed,
}

// NormalizeEventType maps a client-supplied name onto the canonical one.
// Returns ("", false) for a name that is neither canonical nor a known legacy
// alias, so handlers can reject it instead of writing a row nothing will group.
func NormalizeEventType(name string) (string, bool) {
	if ValidEventType(name) {
		return name, true
	}
	if canonical, ok := legacyEventNames[name]; ok {
		return canonical, true
	}
	return "", false
}

// ValidEventType reports whether name is a canonical event.
func ValidEventType(name string) bool {
	_, ok := validEventTypes[name]
	return ok
}

// AllowsAnonymous reports whether name may be recorded against an anon_id
// alone. Callers must still reject a row carrying neither identifier.
func AllowsAnonymous(name string) bool {
	_, ok := anonymousEventTypes[name]
	return ok
}

// EventTypes returns every canonical event name. Used by tests and by the
// analytics dashboard to label rows it has never seen.
func EventTypes() []string {
	out := make([]string, 0, len(validEventTypes))
	for k := range validEventTypes {
		out = append(out, k)
	}
	return out
}
