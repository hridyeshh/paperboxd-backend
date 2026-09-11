package service

import "fmt"

// ReasonEngine builds human-readable recommendation reasons from candidate signals.
type ReasonEngine struct{}

// ReasonResult carries the display text and machine-readable type.
type ReasonResult struct {
	Text string
	Type string
}

// Build returns a ReasonResult for a candidate.
// query is non-empty only for vibe search results.
//
// Priority: vibe → social → twins → recent → trait → anchor (loved / 5★ / TBR)
// → velocity → diary → author → genre → trending → popular → favorites.
// Most specific real signal first; every sentence names evidence that
// actually exists on the reader's shelf or graph. Never fabricate a reason.
//
// CRITICAL: The text strings for social, author, genre, favorites MUST match the
// exact patterns in app/api/books/personalized/route.ts filterBySource:
//
//	social:    Text includes 'read this'
//	author:    Text starts with 'You read'
//	genre:     Text starts with 'Matches your'
//	favorites: Text equals 'Picked for you'
//	explore:   Text equals 'Something different' (set externally, not here)
func (re *ReasonEngine) Build(c Candidate, profile *UserSignalProfile, query string) ReasonResult {
	// Rule 1: Vibe search — query context is the primary signal
	if query != "" {
		display := query
		if len(query) > 40 {
			display = query[:37] + "..."
		}
		if c.FinalScore > 0.85 {
			return ReasonResult{
				Text: fmt.Sprintf("Strong match for \"%s\"", display),
				Type: "vibe",
			}
		}
		return ReasonResult{
			Text: fmt.Sprintf("Matches \"%s\"", display),
			Type: "vibe",
		}
	}

	// Rule 2: Social — a friend read or liked this book.
	// "loved" only when every friend who shelved it liked it; otherwise the
	// honest verb is "read". Type is what the friends tab filters on.
	if c.SocialScore > 0 && len(c.FriendNames) > 0 {
		verb := "read this"
		if c.FriendLovedCount >= len(c.FriendNames) {
			verb = "loved this"
		}
		return ReasonResult{Text: withTraitTail(namesLine(c.FriendNames, "")+" "+verb, c, profile), Type: "social"}
	}

	// Rule 2b: Taste twins — strangers who read like you rated it 4+.
	if c.TwinCount > 0 {
		var text string
		switch {
		case c.TwinCount <= 2 && len(c.TwinNames) >= c.TwinCount:
			text = namesLine(c.TwinNames[:c.TwinCount], "@") + " loved this"
		case c.TwinCount >= 5:
			text = "Readers who rate books like you do are obsessed with this"
		default:
			text = fmt.Sprintf("%d readers with your taste loved this", c.TwinCount)
		}
		return ReasonResult{Text: withTraitTail(text, c, profile), Type: "people_like_you"}
	}

	// Rule 3: Traits — the reader's own shape, matched by this book.
	//
	// Placed above velocity, diary and genre because it is the only reason that
	// says something specific about the *reader*. "Matches your taste for
	// Fiction" is a category label; "you tend to love quiet, character-driven
	// stories" is the recognition the whole roadmap is aimed at.
	//
	// buildTraitReason returns "" unless the reader has a confident opinion on
	// an axis and this book actually sits on it, so this rule is silent rather
	// than vague when the evidence is thin.
	// Recent drift first: when the reader has moved and the book is where
	// they moved to, that is the more specific — and more surprising — fact.
	if profile != nil && c.RecentFitScore > 0 {
		if text := buildRecentReason(c, profile.Traits, profile.RecentTraits); text != "" {
			return ReasonResult{Text: text, Type: "recent"}
		}
	}
	if text := buildTraitReason(c, profile.traits()); text != "" {
		return ReasonResult{Text: text, Type: "trait"}
	}

	// Rule 3a: Antidote — the reader has been going somewhere heavy and this
	// book is the opposite. Only when the recent profile is confident on the
	// axis and the book is clearly on the light side of it.
	if profile != nil && profile.RecentTraits != nil && c.Traits != nil {
		for _, axis := range []string{"darkness", "emotional_intensity"} {
			if profile.RecentTraits.Prefs[axis] >= 0.7 && profile.RecentTraits.Confidence[axis] >= 0.5 && c.Traits[axis] <= 0.3 {
				return ReasonResult{Text: "You've been reading heavier books lately. Maybe you need this", Type: "recent"}
			}
		}
	}

	// Rule 3b: Anchor — this book sits next to one specific book on the
	// reader's shelf. The roadmap's "because you loved X", with the X real.
	switch c.AnchorKind {
	case AnchorRated5, AnchorLoved:
		if c.AnchorCount >= 3 {
			return ReasonResult{Text: fmt.Sprintf("Connects %d books you rated highly, including %s", c.AnchorCount, c.AnchorTitle), Type: "because_loved"}
		}
		if c.AnchorKind == AnchorRated5 {
			return ReasonResult{Text: "Because you rated " + c.AnchorTitle + " 5★", Type: "because_loved"}
		}
		return ReasonResult{Text: "Because you loved " + c.AnchorTitle, Type: "because_loved"}
	case AnchorTBR:
		// "You keep saving books like this but never starting them" — the
		// shorter one is the honest nudge, and only when it actually is.
		if c.PageCount > 0 && c.PageCount <= 250 {
			return ReasonResult{Text: "Like " + c.AnchorTitle + " on your TBR, but shorter", Type: "tbr_similar"}
		}
		return ReasonResult{Text: "Similar to " + c.AnchorTitle + " on your TBR", Type: "tbr_similar"}
	}

	// Rule 3c: New author, strong fit — "you haven't discovered this author
	// yet, but I think they're very you". Requires a confident trait match
	// and an author with no history on the shelf.
	if c.HasTraitFit && c.TraitFitScore >= 0.8 && len(c.Authors) > 0 && profile != nil && !profile.knowsAuthor(c.Authors) {
		return ReasonResult{Text: "You haven't read " + c.Authors[0] + " yet, but they feel very you", Type: "trait"}
	}

	// Rule 4: Velocity — fast-finish centroid match (only non-zero with ranking_v2)
	if c.VelocityBoost > 0.05 {
		if profile != nil && profile.VelocitySignal != nil &&
			profile.VelocitySignal.VelocityBucket == "fast" {
			return ReasonResult{
				Text: "You tend to devour books like this",
				Type: "velocity",
			}
		}
		return ReasonResult{
			Text: "Matches your reading intensity",
			Type: "velocity",
		}
	}

	// Rule 5: Diary — emotional fingerprint match (only non-zero with ranking_v2)
	if c.DiaryBoost > 0.04 {
		return ReasonResult{
			Text: "Matches how you write about books you love",
			Type: "diary",
		}
	}

	// Rule 6: Author weight — the reader has rated this author before.
	// Type "author" is what the authors tab filters on.
	if profile != nil && profile.AuthorWeights != nil {
		for _, author := range c.Authors {
			if w, ok := profile.AuthorWeights[author]; ok && w > 0.3 {
				return ReasonResult{
					Text: fmt.Sprintf("You've loved %s before. Here's another of theirs", author),
					Type: "author",
				}
			}
		}
	}

	// Rule 7: Genre weight — matches user's taste profile
	// Text MUST start with 'Matches your' for the genres tab filter.
	if profile != nil && profile.GenreWeights != nil {
		bestGenre := ""
		bestWeight := 0.0
		for _, cat := range c.Categories {
			if w, ok := profile.GenreWeights[cat]; ok && w > bestWeight {
				bestGenre = cat
				bestWeight = w
			}
		}
		if bestGenre != "" && bestWeight > 0.2 {
			return ReasonResult{
				Text: fmt.Sprintf("Matches your taste for %s", bestGenre),
				Type: "genre",
			}
		}
	}

	// Rule 8: Trending / popularity — community signal, no personal claim.
	if c.IsTrending {
		return ReasonResult{Text: "Trending on Paperboxd this week", Type: "trending"}
	}
	if c.TotalReads >= popularShelfCount {
		return ReasonResult{Text: "Popular right now", Type: "cold"}
	}

	// Rule 9: Pure vector match — no stronger signal.
	// Text MUST equal 'Picked for you' for the favorites tab filter.
	return ReasonResult{Text: "Picked for you", Type: "favorites"}
}

// namesLine renders one, two, or "X and N others" with an optional prefix.
func namesLine(names []string, prefix string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return prefix + names[0]
	case 2:
		return prefix + names[0] + " and " + prefix + names[1]
	default:
		return fmt.Sprintf("%s%s and %d others", prefix, names[0], len(names)-1)
	}
}

// withTraitTail appends the reader's own shape to a social sentence when
// this book matches it: "Maya loved this — and you tend to love quiet,
// character-driven stories". Two independent facts, both true, which is
// what makes the line feel specific rather than clever.
func withTraitTail(text string, c Candidate, profile *UserSignalProfile) string {
	phrases := matchedTraitPhrases(c, profile.traits(), 1)
	if len(phrases) == 0 {
		return text
	}
	return text + " — and you tend to love " + phrases[0]
}
