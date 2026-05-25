package service

import (
	"math"
	"time"
)

// VelocitySignal captures reading speed behaviour for a user.
type VelocitySignal struct {
	AvgDaysToFinish    float64            `json:"avg_days_to_finish"`
	VelocityBucket     string             `json:"velocity_bucket"` // "fast"(<7d), "medium"(7-21d), "slow"(>21d)
	GenreVelocity      map[string]float64 `json:"genre_velocity,omitempty"`
	FastFinishBookIDs  []string           `json:"fast_finish_book_ids,omitempty"`
	AbandonedBookIDs   []string           `json:"abandoned_book_ids,omitempty"`
	ComputedFromNBooks int                `json:"computed_from_n_books"`
}

// computeVelocitySignal derives reading-speed signals from bookshelf entries.
// Abandoned = status "reading", started_at non-null, finished_at IS NULL, started > 60 days ago.
func computeVelocitySignal(books []BookshelfEntry) VelocitySignal {
	sig := VelocitySignal{GenreVelocity: map[string]float64{}}

	var totalDays float64
	genreDays := map[string][]float64{}
	genreCount := map[string]int{}

	for _, b := range books {
		if b.FinishedAt == nil {
			// Check abandoned: reading, started > 60 days ago
			if b.Status == "reading" {
				startedAt := b.CreatedAt
				if time.Since(startedAt) > 60*24*time.Hour {
					sig.AbandonedBookIDs = append(sig.AbandonedBookIDs, b.BookID)
				}
			}
			continue
		}
		startedAt := b.CreatedAt
		days := b.FinishedAt.Sub(startedAt).Hours() / 24
		if days < 0 {
			days = 0
		}
		totalDays += days
		sig.ComputedFromNBooks++

		if days < 7 {
			sig.FastFinishBookIDs = append(sig.FastFinishBookIDs, b.BookID)
		}
		for _, g := range b.Categories {
			genreDays[g] = append(genreDays[g], days)
			genreCount[g]++
		}
	}

	if sig.ComputedFromNBooks > 0 {
		sig.AvgDaysToFinish = totalDays / float64(sig.ComputedFromNBooks)
	}

	switch {
	case sig.AvgDaysToFinish < 7:
		sig.VelocityBucket = "fast"
	case sig.AvgDaysToFinish <= 21:
		sig.VelocityBucket = "medium"
	default:
		sig.VelocityBucket = "slow"
	}

	for genre, days := range genreDays {
		var sum float64
		for _, d := range days {
			sum += d
		}
		sig.GenreVelocity[genre] = sum / float64(genreCount[genre])
	}

	return sig
}

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

// UserSignalProfile holds computed, normalized genre/author preferences and velocity signals.
type UserSignalProfile struct {
	UserID              string
	GenreWeights        map[string]float64
	AuthorWeights       map[string]float64
	VelocitySignal      *VelocitySignal
	DiaryEmbedding      []float32 // mean of embedded diary entries; nil = cold-start
	FastFinishEmbedding []float32 // mean of fast-finish book embeddings; nil = cold-start
	ComputedAt          time.Time
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

	vel := computeVelocitySignal(books)
	return UserSignalProfile{
		UserID:         books[0].UserID,
		GenreWeights:   normalizeWeights(genreWeights),
		AuthorWeights:  normalizeWeights(authorWeights),
		VelocitySignal: &vel,
		ComputedAt:     time.Now(),
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
