package middleware

import "testing"

func TestIsProfileRoot(t *testing.T) {
	cases := []struct {
		path     string
		username string
		want     bool
	}{
		{"/api/v1/users/alice", "alice", true},
		{"/api/v1/users/alice/", "alice", true},
		{"/api/v1/users/Alice", "alice", true}, // chi keeps the raw case
		{"/api/v1/users/alice/diary", "alice", false},
		{"/api/v1/users/alice/bookshelf", "alice", false},
		{"/api/v1/users/alice/lists/123", "alice", false},
		{"/api/v1/users/alice/followers", "alice", false},
		// A username appearing deeper in the path must not open the gate.
		{"/api/v1/users/alice/lists/alice", "alice", false},
		{"/api/v1/users/alice/bookshelf/alice", "alice", false},
		// Nor may a "users" segment elsewhere in the path.
		{"/api/v1/lists/users/alice/diary", "alice", false},
	}
	for _, c := range cases {
		if got := isProfileRoot(c.path, c.username); got != c.want {
			t.Errorf("isProfileRoot(%q, %q) = %v, want %v", c.path, c.username, got, c.want)
		}
	}
}
