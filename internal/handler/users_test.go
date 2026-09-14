package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// The stub a stranger gets for a private account used to serialise
// links/favorite_genres as null, which the iOS decoder rejects, so the profile
// never opened and the Follow button was unreachable.
func TestRedactPrivateProfileKeepsArraysAndDropsReading(t *testing.T) {
	full := types.UserResponse{
		ID: "u1", Username: "alice", Name: "Alice",
		Links: []string{"https://example.com"}, FavoriteGenres: []string{"horror"},
		BooksReadCount: 40, ThoughtsCount: 12, FollowersCount: 3,
	}

	out, err := json.Marshal(redactPrivateProfile(full, false, true))
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)

	for _, want := range []string{`"links":[]`, `"favorite_genres":[]`, `"pronouns":[]`, `"has_requested":true`, `"followers_count":3`} {
		if !strings.Contains(body, want) {
			t.Errorf("stub missing %s: %s", want, body)
		}
	}
	for _, leak := range []string{`example.com`, `horror`, `"books_read_count":40`, `"thoughts_count":12`} {
		if strings.Contains(body, leak) {
			t.Errorf("stub leaks %s: %s", leak, body)
		}
	}
}
