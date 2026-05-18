package service

import (
	"math"
	"time"
)

// Signal weights for explicit vs implicit signals.
const (
	SignalRating5   = 5.0
	SignalRating4   = 4.0
	SignalRating3   = 2.0
	SignalRating1_2 = 0.5  // negative signal
	SignalLiked     = 4.0
	SignalRead      = 2.0  // implicit — decays
	SignalTBR       = 1.0  // implicit intent — decays faster

	DecayLambda    = 0.008 // per day; read half-life ≈ 87 days
	TBRDecayLambda = 0.02  // per day; TBR half-life ≈ 35 days
)

// BookshelfEntry is the input to signal computation.
type BookshelfEntry struct {
	UserID     string
	BookID     string
	Status     string
	Rating     *int
	Authors    []string
	Categories []string
	FinishedAt *time.Time
	UpdatedAt  time.Time
	CreatedAt  time.Time
}

// UserSignalProfile holds computed, normalized genre/author preferences.
type UserSignalProfile struct {
	UserID        string
	GenreWeights  map[string]float64
	AuthorWeights map[string]float64
	ComputedAt    time.Time
}

// DecayedWeight applies exponential decay to an implicit signal.
// Explicit signals (ratings, likes) should not pass through this function.
func DecayedWeight(baseScore float64, eventTime time.Time, lambda float64) float64 {
	daysElapsed := time.Since(eventTime).Hours() / 24
	return baseScore * math.Exp(-lambda*daysElapsed)
}

// SignalWeight returns the weighted score for a bookshelf entry.
// Explicit signals (rating, like) never decay. Implicit signals decay over time.
func SignalWeight(rating *int, status string, interactionTime time.Time) float64 {
	if rating != nil {
		switch *rating {
		case 5:
			return SignalRating5
		case 4:
			return SignalRating4
		case 3:
			return SignalRating3
		case 1, 2:
			return SignalRating1_2
		}
	}
	switch status {
	case "liked":
		return SignalLiked // explicit even without numeric rating
	case "read":
		return DecayedWeight(SignalRead, interactionTime, DecayLambda)
	case "pending":
		return DecayedWeight(SignalTBR, interactionTime, TBRDecayLambda)
	}
	return 0
}

// ComputeUserSignalProfile builds genre and author weight maps from a user's
// bookshelf with temporal decay applied to implicit signals.
func ComputeUserSignalProfile(books []BookshelfEntry) UserSignalProfile {
	genreWeights := map[string]float64{}
	authorWeights := map[string]float64{}

	for _, entry := range books {
		interactionTime := entry.CreatedAt
		if entry.FinishedAt != nil {
			interactionTime = *entry.FinishedAt
		} else if !entry.UpdatedAt.IsZero() {
			interactionTime = entry.UpdatedAt
		}

		weight := SignalWeight(entry.Rating, entry.Status, interactionTime)
		if weight <= 0 {
			continue
		}

		for _, genre := range entry.Categories {
			genreWeights[genre] += weight
		}
		for _, author := range entry.Authors {
			authorWeights[author] += weight
		}
	}

	return UserSignalProfile{
		UserID:        books[0].UserID,
		GenreWeights:  normalizeWeights(genreWeights),
		AuthorWeights: normalizeWeights(authorWeights),
		ComputedAt:    time.Now(),
	}
}

// normalizeWeights scales all values so the maximum is 1.0.
func normalizeWeights(weights map[string]float64) map[string]float64 {
	if len(weights) == 0 {
		return weights
	}
	maxVal := 0.0
	for _, v := range weights {
		if v > maxVal {
			maxVal = v
		}
	}
	if maxVal == 0 {
		return weights
	}
	normalized := make(map[string]float64, len(weights))
	for k, v := range weights {
		normalized[k] = v / maxVal
	}
	return normalized
}
