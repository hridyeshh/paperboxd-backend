package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/hridyesh/paperboxd-backend/internal/db"
)

// The public user view goes to anyone (profiles, search, follower lists), so
// it must never carry the email or birthday; only the owner's own view does.
func TestUserResponsePrivateFieldsOnlyForSelf(t *testing.T) {
	u := db.User{ID: uuid.New(), Username: "reader", Email: "reader@example.com",
		Birthday: pgtype.Date{Time: time.Date(2001, 2, 3, 0, 0, 0, 0, time.UTC), Valid: true}}

	public, _ := json.Marshal(userToResponse(u))
	for _, leak := range []string{"reader@example.com", `"email"`, "2001-02-03", `"birthday"`} {
		if strings.Contains(string(public), leak) {
			t.Errorf("public response leaks %s: %s", leak, public)
		}
	}

	self := selfUserResponse(u)
	if self.Email != u.Email {
		t.Errorf("self response email = %q, want %q", self.Email, u.Email)
	}
	if self.Birthday == nil || *self.Birthday != "2001-02-03" {
		t.Errorf("self response birthday = %v, want 2001-02-03", self.Birthday)
	}
}
