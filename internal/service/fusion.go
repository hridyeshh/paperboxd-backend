package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Fusion persistence: one-time invite links, the pair row, and the cached
// story snapshot. The story itself is built in fusion_view.go.

const (
	fusionInviteTTL = 7 * 24 * time.Hour
	// A snapshot is rebuilt when either shelf changed since, or after this
	// long regardless (trait profiles and recommendations move nightly).
	fusionSnapshotTTL = 24 * time.Hour
	// 32 symbols, no 0/o/1/l lookalikes; 12 of them is 60 bits.
	fusionTokenAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	fusionTokenLen      = 12
)

var (
	ErrFusionInviteUnavailable = errors.New("fusion invite unavailable")
	ErrFusionInviteExpired     = errors.New("fusion invite expired")
	ErrFusionInviteUsed        = errors.New("fusion invite already used")
	ErrFusionInviteOwn         = errors.New("cannot accept your own fusion invite")
	ErrFusionNotFound          = errors.New("fusion not found")
)

// FusionInvite is a live link. URL is filled by the handler.
type FusionInvite struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Invite statuses, as the join page and apps branch on them.
const (
	FusionInviteValid       = "valid"
	FusionInviteExpired     = "expired"
	FusionInviteUsed        = "used"
	FusionInviteOwn         = "own"
	FusionInviteJoined      = "joined" // the viewer already accepted this one
	FusionInviteUnavailable = "unavailable"
)

// FusionInvitePreview is what someone opening a link is shown before Fuse.
// Inviter is omitted when unavailable so a cancelled link or a block reveals
// nothing.
type FusionInvitePreview struct {
	Status    string        `json:"status"`
	Inviter   *FusionPerson `json:"inviter,omitempty"`
	FusionID  string        `json:"fusion_id,omitempty"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
}

// FusionSummary is one row of the reader's Fusions list.
type FusionSummary struct {
	ID        string       `json:"id"`
	Them      FusionPerson `json:"them"`
	Score     *int         `json:"score"` // nil until first built
	CreatedAt time.Time    `json:"created_at"`
}

// FusionList is the profile section: a waiting link, if any, and the Fusions.
type FusionList struct {
	Invite  *FusionInvite   `json:"invite"`
	Fusions []FusionSummary `json:"fusions"`
}

// CreateFusionInvite returns the reader's live link, making one if needed.
func (s *RecommendationService) CreateFusionInvite(ctx context.Context, userID string) (FusionInvite, error) {
	live, err := s.liveFusionInvite(ctx, userID)
	if err != nil {
		return FusionInvite{}, err
	}
	if live != nil {
		return *live, nil
	}
	token, err := newFusionToken()
	if err != nil {
		return FusionInvite{}, err
	}
	inv := FusionInvite{Token: token, ExpiresAt: time.Now().Add(fusionInviteTTL)}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO fusion_invites (token, inviter_id, expires_at) VALUES ($1, $2, $3)
	`, token, userID, inv.ExpiresAt)
	return inv, err
}

// CancelFusionInvite retires an unused link. Idempotent.
func (s *RecommendationService) CancelFusionInvite(ctx context.Context, userID, token string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE fusion_invites SET cancelled_at = NOW()
		WHERE token = $1 AND inviter_id = $2 AND consumed_at IS NULL AND cancelled_at IS NULL
	`, token, userID)
	return err
}

func (s *RecommendationService) liveFusionInvite(ctx context.Context, userID string) (*FusionInvite, error) {
	var inv FusionInvite
	err := s.pool.QueryRow(ctx, `
		SELECT token, expires_at FROM fusion_invites
		WHERE inviter_id = $1 AND consumed_at IS NULL AND cancelled_at IS NULL AND expires_at > NOW()
		ORDER BY created_at DESC LIMIT 1
	`, userID).Scan(&inv.Token, &inv.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// PreviewFusionInvite describes a link for whoever opened it. viewerID may be
// empty for a signed-out visitor.
func (s *RecommendationService) PreviewFusionInvite(ctx context.Context, token, viewerID string) (FusionInvitePreview, error) {
	var (
		inviter    FusionPerson
		expiresAt  time.Time
		cancelled  bool
		consumed   bool
		consumedBy string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.id::text, u.username, COALESCE(u.name, ''), COALESCE(u.avatar_url, ''),
		       i.expires_at, i.cancelled_at IS NOT NULL, i.consumed_at IS NOT NULL,
		       COALESCE(i.consumed_by::text, '')
		FROM fusion_invites i
		JOIN users u ON u.id = i.inviter_id AND u.deleted_at IS NULL
		WHERE i.token = $1
	`, token).Scan(&inviter.UserID, &inviter.Username, &inviter.Name, &inviter.AvatarURL,
		&expiresAt, &cancelled, &consumed, &consumedBy)
	unavailable := FusionInvitePreview{Status: FusionInviteUnavailable}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && cancelled) {
		return unavailable, nil
	}
	if err != nil {
		return unavailable, err
	}
	if viewerID != "" && viewerID != inviter.UserID {
		if blocked, err := s.fusionBlocked(ctx, s.pool, viewerID, inviter.UserID); err != nil || blocked {
			return unavailable, err
		}
	}

	inviter.First = firstName(inviter.Name, inviter.Username)
	p := FusionInvitePreview{Inviter: &inviter, ExpiresAt: &expiresAt}
	switch {
	case viewerID != "" && viewerID == inviter.UserID:
		p.Status = FusionInviteOwn
	case consumed && viewerID != "" && consumedBy == viewerID:
		p.Status = FusionInviteJoined
		p.FusionID, _ = s.fusionIDForPair(ctx, viewerID, inviter.UserID)
	case consumed:
		p.Status = FusionInviteUsed
	case time.Now().After(expiresAt):
		p.Status = FusionInviteExpired
	default:
		p.Status = FusionInviteValid
	}
	return p, nil
}

// AcceptFusionInvite spends the link and returns the pair's Fusion id. The row
// lock makes "first to accept wins" hold under concurrent taps. Accepting a
// link that pairs two readers who already have a Fusion returns that one.
func (s *RecommendationService) AcceptFusionInvite(ctx context.Context, token, userID string) (string, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return "", ErrFusionInviteUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var (
		inviter    uuid.UUID
		expiresAt  time.Time
		cancelled  bool
		consumed   bool
		consumedBy *uuid.UUID
	)
	err = tx.QueryRow(ctx, `
		SELECT i.inviter_id, i.expires_at, i.cancelled_at IS NOT NULL, i.consumed_at IS NOT NULL, i.consumed_by
		FROM fusion_invites i
		JOIN users u ON u.id = i.inviter_id AND u.deleted_at IS NULL
		WHERE i.token = $1
		FOR UPDATE OF i
	`, token).Scan(&inviter, &expiresAt, &cancelled, &consumed, &consumedBy)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrFusionInviteUnavailable
	case err != nil:
		return "", err
	case cancelled:
		return "", ErrFusionInviteUnavailable
	case inviter == uid:
		return "", ErrFusionInviteOwn
	case consumed && consumedBy != nil && *consumedBy == uid:
		return s.fusionIDForPair(ctx, userID, inviter.String())
	case consumed:
		return "", ErrFusionInviteUsed
	case time.Now().After(expiresAt):
		return "", ErrFusionInviteExpired
	}
	if blocked, err := s.fusionBlocked(ctx, tx, userID, inviter.String()); err != nil {
		return "", err
	} else if blocked {
		return "", ErrFusionInviteUnavailable
	}

	a, b := orderPair(uid, inviter)
	var fusionID uuid.UUID
	// The no-op DO UPDATE makes RETURNING yield the existing row on conflict.
	if err := tx.QueryRow(ctx, `
		INSERT INTO fusions (user_a, user_b, inviter_id) VALUES ($1, $2, $3)
		ON CONFLICT (user_a, user_b) DO UPDATE SET user_a = EXCLUDED.user_a
		RETURNING id
	`, a, b, inviter).Scan(&fusionID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE fusion_invites SET consumed_at = NOW(), consumed_by = $2 WHERE token = $1
	`, token, uid); err != nil {
		return "", err
	}
	meta, _ := json.Marshal(map[string]string{"fusion_id": fusionID.String()})
	if _, err := tx.Exec(ctx, `
		INSERT INTO activities (user_id, activity_type, target_user_id, metadata)
		VALUES ($1, 'fusion_joined', $2, $3)
	`, uid, inviter, meta); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return fusionID.String(), nil
}

// ListFusions returns the reader's waiting link and Fusions, newest first.
func (s *RecommendationService) ListFusions(ctx context.Context, userID string) (FusionList, error) {
	list := FusionList{Fusions: []FusionSummary{}}
	inv, err := s.liveFusionInvite(ctx, userID)
	if err != nil {
		return list, err
	}
	list.Invite = inv

	rows, err := s.pool.Query(ctx, `
		SELECT f.id::text, f.created_at, (f.snapshot->'a'->>'score')::int,
		       u.id::text, u.username, COALESCE(u.name, ''), COALESCE(u.avatar_url, '')
		FROM fusions f
		JOIN users u ON u.id = CASE WHEN f.user_a = $1 THEN f.user_b ELSE f.user_a END
		            AND u.deleted_at IS NULL
		WHERE (f.user_a = $1 OR f.user_b = $1)
		  AND NOT EXISTS (
		      SELECT 1 FROM blocks bl
		      WHERE (bl.blocker_id = $1 AND bl.blocked_id = u.id)
		         OR (bl.blocker_id = u.id AND bl.blocked_id = $1))
		ORDER BY f.created_at DESC
		LIMIT 50
	`, userID)
	if err != nil {
		return list, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FusionSummary
		if err := rows.Scan(&f.ID, &f.CreatedAt, &f.Score,
			&f.Them.UserID, &f.Them.Username, &f.Them.Name, &f.Them.AvatarURL); err != nil {
			return list, err
		}
		f.Them.First = firstName(f.Them.Name, f.Them.Username)
		list.Fusions = append(list.Fusions, f)
	}
	return list, rows.Err()
}

// GetFusion returns the story from userID's side, rebuilding a stale snapshot.
// Not a member, a block since, or a deleted partner all read as not found.
func (s *RecommendationService) GetFusion(ctx context.Context, fusionID, userID string) (FusionView, error) {
	var (
		ua, ub uuid.UUID
		raw    []byte
		stale  bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT f.user_a, f.user_b, f.snapshot,
		       f.computed_at IS NULL
		       OR f.computed_at < NOW() - make_interval(secs => $3)
		       OR f.computed_at < COALESCE(
		           (SELECT MAX(updated_at) FROM bookshelf WHERE user_id IN (f.user_a, f.user_b)),
		           'epoch')
		FROM fusions f
		JOIN users ua ON ua.id = f.user_a AND ua.deleted_at IS NULL
		JOIN users ub ON ub.id = f.user_b AND ub.deleted_at IS NULL
		WHERE f.id = $1 AND $2 IN (f.user_a, f.user_b)
		  AND NOT EXISTS (
		      SELECT 1 FROM blocks bl
		      WHERE (bl.blocker_id = f.user_a AND bl.blocked_id = f.user_b)
		         OR (bl.blocker_id = f.user_b AND bl.blocked_id = f.user_a))
	`, fusionID, userID, fusionSnapshotTTL.Seconds()).Scan(&ua, &ub, &raw, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return FusionView{}, ErrFusionNotFound
	}
	if err != nil {
		return FusionView{}, err
	}

	var snap fusionSnapshot
	haveOld := raw != nil && json.Unmarshal(raw, &snap) == nil
	if stale || !haveOld {
		fresh, err := s.buildFusionSnapshot(ctx, ua, ub)
		switch {
		case err == nil:
			snap = fresh
			if b, err := json.Marshal(snap); err == nil {
				if _, err := s.pool.Exec(ctx, `UPDATE fusions SET snapshot = $2, computed_at = $3 WHERE id = $1`,
					fusionID, b, fresh.A.ComputedAt); err != nil {
					slog.Warn("fusion: save snapshot", "error", err, "fusion_id", fusionID)
				}
			}
		case haveOld:
			// A stale story beats an error page; it rebuilds next open.
			slog.Warn("fusion: rebuild failed, serving previous", "error", err, "fusion_id", fusionID)
		default:
			return FusionView{}, err
		}
	}

	v := snap.A
	if ub.String() == userID {
		v = snap.B
	}
	v.ID = fusionID
	return v, nil
}

// DeleteFusion removes a Fusion for both readers. Either member may.
func (s *RecommendationService) DeleteFusion(ctx context.Context, fusionID, userID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM fusions WHERE id = $1 AND $2 IN (user_a, user_b)`, fusionID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrFusionNotFound
	}
	return nil
}

// buildFusionSnapshot loads both readers and renders the story from each side.
func (s *RecommendationService) buildFusionSnapshot(ctx context.Context, a, b uuid.UUID) (fusionSnapshot, error) {
	ids := []uuid.UUID{a, b}
	in := fusionInput{
		Books:      map[string]fusionBookMeta{},
		BookTraits: map[string]map[string]float64{},
		Now:        time.Now().UTC(),
	}
	sides := map[string]*fusionSide{a.String(): &in.A, b.String(): &in.B}
	for id, side := range sides {
		side.Person.UserID = id
		side.Shelf = readerShelf{UserID: id, Books: map[string]*int{}}
		side.AllIDs = map[string]bool{}
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id::text, username, COALESCE(name, ''), COALESCE(avatar_url, '')
		FROM users WHERE id = ANY($1::uuid[])
	`, ids)
	if err != nil {
		return fusionSnapshot{}, err
	}
	for rows.Next() {
		var p FusionPerson
		if rows.Scan(&p.UserID, &p.Username, &p.Name, &p.AvatarURL) == nil {
			p.First = firstName(p.Name, p.Username)
			sides[p.UserID].Person = p
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fusionSnapshot{}, err
	}

	shelves, err := s.loadShelves(ctx, ids)
	if err != nil {
		return fusionSnapshot{}, err
	}
	for id, sh := range shelves {
		sides[id].Shelf = *sh
	}

	rows, err = s.pool.Query(ctx, `SELECT user_id::text, book_id::text FROM bookshelf WHERE user_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return fusionSnapshot{}, err
	}
	for rows.Next() {
		var uid, bid string
		if rows.Scan(&uid, &bid) == nil {
			sides[uid].AllIDs[bid] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fusionSnapshot{}, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT b.user_id::text, percentile_cont(0.5) WITHIN GROUP (ORDER BY bk.page_count)
		FROM bookshelf b JOIN books bk ON bk.id = b.book_id
		WHERE b.user_id = ANY($1::uuid[]) AND b.status IN ('read', 'reading') AND bk.page_count > 0
		GROUP BY b.user_id
		HAVING COUNT(*) >= 5
	`, ids)
	if err != nil {
		return fusionSnapshot{}, err
	}
	for rows.Next() {
		var uid string
		var median float64
		if rows.Scan(&uid, &median) == nil {
			sides[uid].MedianPages = int(median)
		}
	}
	rows.Close()

	for id, side := range sides {
		// Both are best-effort: a thin reader still gets a story, with the
		// sections that need these left empty.
		if profile, err := s.GetOrComputeSignalProfile(ctx, id); err == nil {
			side.Genres = profile.GenreWeights
			if side.Shelf.Traits == nil && profile.Traits.HasSignal() {
				side.Shelf.Traits = profile.Traits
			}
		} else {
			slog.Warn("fusion: signal profile", "error", err, "user_id", id)
		}
		if recs, _, err := s.GetHomeRecommendations(ctx, id); err == nil {
			side.Recs = recs
		} else {
			slog.Warn("fusion: recommendations", "error", err, "user_id", id)
		}
	}

	in.SharedAuthors = s.sharedAuthorsByUser(ctx, a, []uuid.UUID{b}, 50)[b.String()]

	// Metadata and traits for every book the story could mention: anything
	// either reader rated 4+, anything shared, and both recommendation lists.
	want := map[string]bool{}
	for id, ra := range in.A.Shelf.Books {
		if _, shared := in.B.Shelf.Books[id]; shared || (ra != nil && *ra >= 4) {
			want[id] = true
		}
	}
	for id, rb := range in.B.Shelf.Books {
		if rb != nil && *rb >= 4 {
			want[id] = true
		}
	}
	for _, side := range []fusionSide{in.A, in.B} {
		for _, r := range side.Recs {
			want[r.ID] = true
		}
	}
	bookIDs := make([]string, 0, len(want))
	cands := make([]Candidate, 0, len(want))
	for id := range want {
		bookIDs = append(bookIDs, id)
		cands = append(cands, Candidate{BookID: id})
	}
	rows, err = s.pool.Query(ctx, `
		SELECT id::text, title, COALESCE(slug, ''), COALESCE(authors, '{}'), COALESCE(cover_url, ''), COALESCE(categories, '{}')
		FROM books WHERE id = ANY($1::uuid[])
	`, bookIDs)
	if err != nil {
		return fusionSnapshot{}, err
	}
	for rows.Next() {
		var id string
		var m fusionBookMeta
		if rows.Scan(&id, &m.Title, &m.Slug, &m.Authors, &m.CoverURL, &m.Categories) == nil {
			in.Books[id] = m
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fusionSnapshot{}, err
	}
	if err := s.fetchCandidateTraits(ctx, cands); err != nil {
		slog.Warn("fusion: book traits", "error", err)
	}
	for _, c := range cands {
		if c.Traits != nil {
			in.BookTraits[c.BookID] = c.Traits
		}
	}

	return fusionSnapshot{A: buildFusionView(in), B: buildFusionView(in.flipped())}, nil
}

// pgxQuerier is the QueryRow half of a pool or a transaction.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *RecommendationService) fusionBlocked(ctx context.Context, q pgxQuerier, x, y string) (bool, error) {
	var blocked bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM blocks
		               WHERE (blocker_id = $1 AND blocked_id = $2) OR (blocker_id = $2 AND blocked_id = $1))
	`, x, y).Scan(&blocked)
	return blocked, err
}

func (s *RecommendationService) fusionIDForPair(ctx context.Context, x, y string) (string, error) {
	xa, err1 := uuid.Parse(x)
	ya, err2 := uuid.Parse(y)
	if err1 != nil || err2 != nil {
		return "", ErrFusionNotFound
	}
	a, b := orderPair(xa, ya)
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM fusions WHERE user_a = $1 AND user_b = $2`, a, b).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrFusionNotFound
	}
	return id, err
}

// orderPair returns the pair as stored: the smaller uuid first, matching
// Postgres uuid ordering (byte-wise, same as the canonical string).
func orderPair(x, y uuid.UUID) (uuid.UUID, uuid.UUID) {
	if x.String() < y.String() {
		return x, y
	}
	return y, x
}

func newFusionToken() (string, error) {
	b := make([]byte, fusionTokenLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = fusionTokenAlphabet[b[i]&31]
	}
	return string(b), nil
}

// ValidFusionToken rejects anything that could not have come from
// newFusionToken before it reaches the database.
func ValidFusionToken(t string) bool {
	if len(t) != fusionTokenLen {
		return false
	}
	for _, r := range t {
		if !strings.ContainsRune(fusionTokenAlphabet, r) {
			return false
		}
	}
	return true
}

func firstName(name, username string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return username
}
