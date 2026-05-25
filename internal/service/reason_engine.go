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
// Priority: vibe → social → velocity → diary → author → genre → cold/favorites.
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

	// Rule 2: Social — a friend read or liked this book
	// Text MUST include 'read this' for the friends tab filter.
	if c.SocialScore > 0 && len(c.FriendNames) > 0 {
		name := c.FriendNames[0]
		switch len(c.FriendNames) {
		case 1:
			return ReasonResult{Text: fmt.Sprintf("%s read this", name), Type: "social"}
		case 2:
			return ReasonResult{
				Text: fmt.Sprintf("%s and %s read this", name, c.FriendNames[1]),
				Type: "social",
			}
		default:
			return ReasonResult{
				Text: fmt.Sprintf("%s and %d others read this", name, len(c.FriendNames)-1),
				Type: "social",
			}
		}
	}

	// Rule 3: Velocity — fast-finish centroid match (only non-zero with ranking_v2)
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

	// Rule 4: Diary — emotional fingerprint match (only non-zero with ranking_v2)
	if c.DiaryBoost > 0.04 {
		return ReasonResult{
			Text: "Matches how you write about books you love",
			Type: "diary",
		}
	}

	// Rule 5: Author weight — user has engaged with this author
	// Text MUST start with 'You read' for the authors tab filter.
	if profile != nil && profile.AuthorWeights != nil {
		for _, author := range c.Authors {
			if w, ok := profile.AuthorWeights[author]; ok && w > 0.3 {
				return ReasonResult{
					Text: fmt.Sprintf("You read more by %s", author),
					Type: "author",
				}
			}
		}
	}

	// Rule 6: Genre weight — matches user's taste profile
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

	// Rule 7: Trending / popularity signal
	if c.LikeCount+c.TotalReads > 50 {
		return ReasonResult{Text: "Popular right now", Type: "cold"}
	}

	// Rule 8: Pure vector match — no stronger signal.
	// Text MUST equal 'Picked for you' for the favorites tab filter.
	return ReasonResult{Text: "Picked for you", Type: "favorites"}
}
