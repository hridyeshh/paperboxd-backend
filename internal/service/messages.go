package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Messages: 1:1 DMs with Paperboxd objects as attachments. Schema and the
// reasoning behind it live in migrations/000057 and docs/MESSAGES.md.
//
// ponytail: delivery is polling (clients ask for ?after=<id>). Push comes
// once the store accounts exist; Send is the one place it plugs in.

const (
	MaxMessageBody        = 2000
	MaxMessageAttachments = 10
	MaxMessageRecipients  = 5
	// Until a request is accepted, the sender gets this many messages in.
	requestMessageCap = 3
	// New conversations a reader may start per rolling day.
	dailyConversationCap = 20
	messagePageSize      = 50
	conversationPageSize = 30
	thoughtExcerptLen    = 280
)

// Attachment kinds, matching the CHECK in migrations/000057.
const (
	AttachBook    = "book"
	AttachList    = "list"
	AttachThought = "thought"
	AttachProfile = "profile"
	AttachFusion  = "fusion"
)

// Member states.
const (
	ConvAccepted = "accepted"
	ConvRequest  = "request"
)

var (
	ErrConversationNotFound  = errors.New("conversation not found")
	ErrMessageNotFound       = errors.New("message not found")
	ErrRecipientNotFound     = errors.New("recipient not found")
	ErrCannotMessage         = errors.New("cannot message this reader")
	ErrRequestLimit          = errors.New("request limit reached")
	ErrDailyConversationCap  = errors.New("daily new conversation limit reached")
	ErrAttachmentUnavailable = errors.New("attachment unavailable")
	ErrInvalidMessage        = errors.New("invalid message")
)

// MessagePerson is the other side of a conversation.
type MessagePerson struct {
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// AttachmentRef is what a client sends: a kind and the object's id.
type AttachmentRef struct {
	Kind  string `json:"kind"`
	RefID string `json:"ref_id"`
}

// MessageAttachment is an attachment as stored, with its send-time card.
type MessageAttachment struct {
	Kind     string          `json:"kind"`
	RefID    string          `json:"ref_id"`
	Snapshot json.RawMessage `json:"snapshot"`
}

type Message struct {
	ID             int64               `json:"id"`
	ConversationID string              `json:"conversation_id"`
	SenderID       string              `json:"sender_id"`
	FromMe         bool                `json:"from_me"`
	Body           string              `json:"body"`
	CreatedAt      time.Time           `json:"created_at"`
	Unsent         bool                `json:"unsent"`
	Attachments    []MessageAttachment `json:"attachments"`
}

type MessagePreview struct {
	Text      string    `json:"text"`
	FromMe    bool      `json:"from_me"`
	CreatedAt time.Time `json:"created_at"`
}

// ConversationSummary is one inbox row.
type ConversationSummary struct {
	ID          string         `json:"id"`
	State       string         `json:"state"`
	Muted       bool           `json:"muted"`
	Them        MessagePerson  `json:"them"`
	LastMessage MessagePreview `json:"last_message"`
	UnreadCount int            `json:"unread_count"`
}

type ConversationList struct {
	Conversations []ConversationSummary `json:"conversations"`
	// Pass back as ?before= for the next page; empty when there is none.
	NextCursor string `json:"next_cursor"`
}

// ConversationPage is a chat screen's worth: header, messages oldest first,
// and the other reader's read marker for "Seen".
type ConversationPage struct {
	ID              string        `json:"id"`
	State           string        `json:"state"`
	Muted           bool          `json:"muted"`
	Them            MessagePerson `json:"them"`
	TheirLastReadID int64         `json:"their_last_read_id"`
	Messages        []Message     `json:"messages"`
	HasMore         bool          `json:"has_more"`
}

type SendInput struct {
	ConversationID string
	To             []string // usernames; used when ConversationID is empty
	Body           string
	Attachments    []AttachmentRef
	ClientID       string
}

type SendFailure struct {
	Username string `json:"username"`
	Error    string `json:"error"`
}

type SendResult struct {
	Sent   []Message     `json:"sent"`
	Failed []SendFailure `json:"failed"`
}

type SharedItem struct {
	MessageID int64           `json:"message_id"`
	Kind      string          `json:"kind"`
	RefID     string          `json:"ref_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	FromMe    bool            `json:"from_me"`
	CreatedAt time.Time       `json:"created_at"`
	cursor    int64
}

type SharedList struct {
	Counts     map[string]int `json:"counts"`
	Items      []SharedItem   `json:"items"`
	NextCursor string         `json:"next_cursor"`
}

type InboxBadge struct {
	Unread   int `json:"unread"`   // accepted, unmuted conversations with something new
	Requests int `json:"requests"` // visible message requests
}

type MessageService struct {
	pool   *pgxpool.Pool
	events *EventService
}

func NewMessageService(pool *pgxpool.Pool, events *EventService) *MessageService {
	return &MessageService{pool: pool, events: events}
}

// ── Validation (pure) ────────────────────────────────────────────────────────

func validAttachKind(k string) bool {
	switch k {
	case AttachBook, AttachList, AttachThought, AttachProfile, AttachFusion:
		return true
	}
	return false
}

// validateSend checks the shape of a send before anything touches the DB.
func validateSend(in SendInput) error {
	body := strings.TrimSpace(in.Body)
	if body == "" && len(in.Attachments) == 0 {
		return fmt.Errorf("%w: message is empty", ErrInvalidMessage)
	}
	if utf8.RuneCountInString(in.Body) > MaxMessageBody {
		return fmt.Errorf("%w: message is longer than %d characters", ErrInvalidMessage, MaxMessageBody)
	}
	if len(in.Attachments) > MaxMessageAttachments {
		return fmt.Errorf("%w: at most %d attachments", ErrInvalidMessage, MaxMessageAttachments)
	}
	for _, a := range in.Attachments {
		if !validAttachKind(a.Kind) || strings.TrimSpace(a.RefID) == "" {
			return fmt.Errorf("%w: bad attachment", ErrInvalidMessage)
		}
	}
	if in.ConversationID == "" {
		if len(in.To) == 0 {
			return fmt.Errorf("%w: conversation_id or to is required", ErrInvalidMessage)
		}
		if len(in.To) > MaxMessageRecipients {
			return fmt.Errorf("%w: at most %d recipients", ErrInvalidMessage, MaxMessageRecipients)
		}
	}
	if in.ClientID != "" {
		if _, err := uuid.Parse(in.ClientID); err != nil {
			return fmt.Errorf("%w: client_id must be a uuid", ErrInvalidMessage)
		}
	}
	return nil
}

// initialState is where a new conversation lands for its recipient: straight
// in the inbox when they already follow the sender, else in Requests.
func initialState(recipientFollowsSender bool) string {
	if recipientFollowsSender {
		return ConvAccepted
	}
	return ConvRequest
}

var attachmentPreview = map[string]string{
	AttachBook:    "Sent a book",
	AttachList:    "Sent a list",
	AttachThought: "Sent a thought",
	AttachProfile: "Sent a profile",
	AttachFusion:  "Sent a Fusion",
}

// previewText is the one-line inbox preview for a message.
func previewText(body, firstKind string, unsent bool) string {
	if unsent {
		return "Message unsent"
	}
	if b := strings.Join(strings.Fields(body), " "); b != "" {
		return truncateRunes(b, 120)
	}
	if p, ok := attachmentPreview[firstKind]; ok {
		return p
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// ── Send ─────────────────────────────────────────────────────────────────────

type builtAttachment struct {
	kind     string
	refID    string
	snapshot []byte
	// Set for the sender's own private list: sending it grants the recipient
	// read access, the same as the existing list share.
	grantList *uuid.UUID
}

// Send delivers one message to an existing conversation or to up to five
// readers. Book ref_ids must already be resolved to Paperboxd UUIDs (the
// handler does that, since it owns the Google Books / ISBNdb clients).
// Per-recipient failures are reported, not fatal; attachment and shape errors
// fail the whole send.
func (s *MessageService) Send(ctx context.Context, senderID string, in SendInput) (SendResult, error) {
	res := SendResult{Sent: []Message{}, Failed: []SendFailure{}}
	if err := validateSend(in); err != nil {
		return res, err
	}
	sender, err := uuid.Parse(senderID)
	if err != nil {
		return res, ErrCannotMessage
	}
	atts := make([]builtAttachment, 0, len(in.Attachments))
	for _, a := range in.Attachments {
		b, err := s.buildAttachment(ctx, sender, a)
		if err != nil {
			return res, err
		}
		atts = append(atts, b)
	}

	if in.ConversationID != "" {
		convID, err := uuid.Parse(in.ConversationID)
		if err != nil {
			return res, ErrConversationNotFound
		}
		other, err := s.otherMember(ctx, convID, sender)
		if err != nil {
			return res, err
		}
		msg, err := s.sendOne(ctx, sender, other, in, atts)
		if err != nil {
			return res, err
		}
		res.Sent = append(res.Sent, msg)
	} else {
		seen := map[string]bool{}
		for _, raw := range in.To {
			uname := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
			if uname == "" || seen[uname] {
				continue
			}
			seen[uname] = true
			msg, err := s.sendToUsername(ctx, sender, uname, in, atts)
			if err != nil {
				// A multi-send with a single target should surface the real error.
				if len(in.To) == 1 {
					return res, err
				}
				res.Failed = append(res.Failed, SendFailure{Username: uname, Error: err.Error()})
				continue
			}
			res.Sent = append(res.Sent, msg)
		}
	}

	if len(res.Sent) > 0 && s.events != nil {
		kinds := make([]string, len(atts))
		for i, a := range atts {
			kinds[i] = a.kind
		}
		go s.events.Emit(context.Background(), EmitParams{
			UserID:    sender,
			EventType: EventMessageSent,
			Source:    "server",
			Metadata:  map[string]any{"recipients": len(res.Sent), "attachments": kinds, "has_text": strings.TrimSpace(in.Body) != ""},
		})
	}
	return res, nil
}

func (s *MessageService) sendToUsername(ctx context.Context, sender uuid.UUID, username string, in SendInput, atts []builtAttachment) (Message, error) {
	var recipient uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM users WHERE username = $1 AND deleted_at IS NULL
	`, username).Scan(&recipient)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrRecipientNotFound
	}
	if err != nil {
		return Message{}, err
	}
	if recipient == sender {
		return Message{}, ErrCannotMessage
	}
	return s.sendOne(ctx, sender, recipient, in, atts)
}

// otherMember returns the other reader in a conversation the sender belongs to.
func (s *MessageService) otherMember(ctx context.Context, convID, userID uuid.UUID) (uuid.UUID, error) {
	var a, b uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT c.user_a, c.user_b FROM conversations c
		JOIN conversation_members me ON me.conversation_id = c.id AND me.user_id = $2
		WHERE c.id = $1
	`, convID, userID).Scan(&a, &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrConversationNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	if a == userID {
		return b, nil
	}
	return a, nil
}

func (s *MessageService) sendOne(ctx context.Context, sender, recipient uuid.UUID, in SendInput, atts []builtAttachment) (Message, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback(ctx)

	var (
		blocked          bool
		recipientDeleted bool
		recipientFollows bool
	)
	err = tx.QueryRow(ctx, `
		SELECT
		  EXISTS (SELECT 1 FROM blocks
		          WHERE (blocker_id = $1 AND blocked_id = $2) OR (blocker_id = $2 AND blocked_id = $1)),
		  COALESCE((SELECT deleted_at IS NOT NULL FROM users WHERE id = $2), true),
		  EXISTS (SELECT 1 FROM follows WHERE follower_id = $2 AND following_id = $1)
	`, sender, recipient).Scan(&blocked, &recipientDeleted, &recipientFollows)
	if err != nil {
		return Message{}, err
	}
	if blocked || recipientDeleted {
		return Message{}, ErrCannotMessage
	}

	a, b := orderPair(sender, recipient)
	var convID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM conversations WHERE user_a = $1 AND user_b = $2`, a, b).Scan(&convID)
	if errors.Is(err, pgx.ErrNoRows) {
		var started int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM conversations
			WHERE created_by = $1 AND created_at > NOW() - INTERVAL '1 day'
		`, sender).Scan(&started); err != nil {
			return Message{}, err
		}
		if started >= dailyConversationCap {
			return Message{}, ErrDailyConversationCap
		}
		// The no-op DO UPDATE makes RETURNING yield the row a concurrent
		// first message may have just created.
		if err := tx.QueryRow(ctx, `
			INSERT INTO conversations (user_a, user_b, created_by) VALUES ($1, $2, $3)
			ON CONFLICT (user_a, user_b) DO UPDATE SET user_a = EXCLUDED.user_a
			RETURNING id
		`, a, b, sender).Scan(&convID); err != nil {
			return Message{}, err
		}
	} else if err != nil {
		return Message{}, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO conversation_members (conversation_id, user_id, state)
		VALUES ($1, $2, 'accepted'), ($1, $3, $4)
		ON CONFLICT (conversation_id, user_id) DO NOTHING
	`, convID, sender, recipient, initialState(recipientFollows)); err != nil {
		return Message{}, err
	}

	// Lock both member rows so the request cap holds under concurrent sends.
	var senderState, recipientState string
	rows, err := tx.Query(ctx, `
		SELECT user_id, state FROM conversation_members
		WHERE conversation_id = $1 FOR UPDATE
	`, convID)
	if err != nil {
		return Message{}, err
	}
	for rows.Next() {
		var uid uuid.UUID
		var st string
		if err := rows.Scan(&uid, &st); err != nil {
			rows.Close()
			return Message{}, err
		}
		if uid == sender {
			senderState = st
		} else {
			recipientState = st
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Message{}, err
	}

	if in.ClientID != "" {
		var existing int64
		err := tx.QueryRow(ctx, `
			SELECT id FROM messages WHERE conversation_id = $1 AND client_id = $2
		`, convID, in.ClientID).Scan(&existing)
		if err == nil {
			_ = tx.Rollback(ctx)
			return s.messageByID(ctx, existing, sender)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Message{}, err
		}
	}

	// Replying to a request accepts it.
	if senderState == ConvRequest {
		if _, err := tx.Exec(ctx, `
			UPDATE conversation_members SET state = 'accepted'
			WHERE conversation_id = $1 AND user_id = $2
		`, convID, sender); err != nil {
			return Message{}, err
		}
	}
	if recipientState == ConvRequest {
		var sent int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM messages WHERE conversation_id = $1 AND sender_id = $2
		`, convID, sender).Scan(&sent); err != nil {
			return Message{}, err
		}
		if sent >= requestMessageCap {
			return Message{}, ErrRequestLimit
		}
	}

	var clientID *string
	if in.ClientID != "" {
		clientID = &in.ClientID
	}
	msg := Message{ConversationID: convID.String(), SenderID: sender.String(), FromMe: true, Body: in.Body, Attachments: []MessageAttachment{}}
	if err := tx.QueryRow(ctx, `
		INSERT INTO messages (conversation_id, sender_id, body, client_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at
	`, convID, sender, in.Body, clientID).Scan(&msg.ID, &msg.CreatedAt); err != nil {
		return Message{}, err
	}
	for i, att := range atts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO message_attachments (message_id, conversation_id, kind, ref_id, snapshot, position)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, msg.ID, convID, att.kind, att.refID, att.snapshot, i); err != nil {
			return Message{}, err
		}
		if att.grantList != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO list_access (list_id, user_id, granted_by) VALUES ($1, $2, $3)
				ON CONFLICT (list_id, user_id) DO NOTHING
			`, *att.grantList, recipient, sender); err != nil {
				return Message{}, err
			}
		}
		msg.Attachments = append(msg.Attachments, MessageAttachment{Kind: att.kind, RefID: att.refID, Snapshot: att.snapshot})
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations SET last_message_id = $2, last_message_at = $3 WHERE id = $1
	`, convID, msg.ID, msg.CreatedAt); err != nil {
		return Message{}, err
	}
	// Your own message is read by definition.
	if _, err := tx.Exec(ctx, `
		UPDATE conversation_members SET last_read_message_id = $3
		WHERE conversation_id = $1 AND user_id = $2
	`, convID, sender, msg.ID); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// ── Attachment snapshots ─────────────────────────────────────────────────────

// buildAttachment checks the sender may share the object and freezes its card.
// Other readers' content is only shareable when their account is public, so a
// card can never show a recipient something the owner has not made public.
func (s *MessageService) buildAttachment(ctx context.Context, sender uuid.UUID, a AttachmentRef) (builtAttachment, error) {
	out := builtAttachment{kind: a.Kind}
	var snap any
	refID := strings.TrimSpace(a.RefID)

	switch a.Kind {
	case AttachBook:
		id, err := uuid.Parse(refID)
		if err != nil {
			return out, ErrAttachmentUnavailable
		}
		var (
			title, slug string
			authors     []string
			cover       *string
		)
		err = s.pool.QueryRow(ctx, `
			SELECT title, COALESCE(slug, ''), COALESCE(authors, '{}'), cover_url FROM books WHERE id = $1
		`, id).Scan(&title, &slug, &authors, &cover)
		if err != nil {
			return out, notFoundAs(err, ErrAttachmentUnavailable)
		}
		out.refID = id.String()
		snap = map[string]any{"title": title, "authors": authors, "cover_url": deref(cover), "slug": slug}

	case AttachList:
		id, err := uuid.Parse(refID)
		if err != nil {
			return out, ErrAttachmentUnavailable
		}
		var (
			title, ownerUsername, ownerName string
			owner                           uuid.UUID
			isPrivate, ownerPublic          bool
			bookCount                       int
			covers                          []string
		)
		err = s.pool.QueryRow(ctx, `
			SELECT l.title, l.is_private, l.user_id, u.username, COALESCE(u.name, ''), u.is_public,
			       (SELECT COUNT(*) FROM list_books lb WHERE lb.list_id = l.id),
			       COALESCE(ARRAY(
			           SELECT b.cover_url FROM list_books lb JOIN books b ON b.id = lb.book_id
			           WHERE lb.list_id = l.id AND b.cover_url IS NOT NULL AND b.cover_url <> ''
			           ORDER BY lb.display_order, lb.added_at LIMIT 3), '{}')
			FROM lists l JOIN users u ON u.id = l.user_id AND u.deleted_at IS NULL
			WHERE l.id = $1
		`, id).Scan(&title, &isPrivate, &owner, &ownerUsername, &ownerName, &ownerPublic, &bookCount, &covers)
		if err != nil {
			return out, notFoundAs(err, ErrAttachmentUnavailable)
		}
		mine := owner == sender
		if !mine && (isPrivate || !ownerPublic) {
			return out, ErrAttachmentUnavailable
		}
		if mine && isPrivate {
			out.grantList = &id
		}
		out.refID = id.String()
		snap = map[string]any{"title": title, "owner_username": ownerUsername, "owner_name": ownerName,
			"book_count": bookCount, "covers": covers, "is_private": isPrivate}

	case AttachThought:
		id, err := uuid.Parse(refID)
		if err != nil {
			return out, ErrAttachmentUnavailable
		}
		var (
			content, username, name string
			author                  uuid.UUID
			isPrivate, authorPublic bool
			rating                  *int32
			bookID                  *uuid.UUID
			bookTitle, bookCover    *string
		)
		err = s.pool.QueryRow(ctx, `
			SELECT t.content, t.is_private, t.rating, t.user_id, u.username, COALESCE(u.name, ''), u.is_public,
			       b.id, b.title, b.cover_url
			FROM thoughts t
			JOIN users u ON u.id = t.user_id AND u.deleted_at IS NULL
			LEFT JOIN books b ON b.id = t.book_id
			WHERE t.id = $1
		`, id).Scan(&content, &isPrivate, &rating, &author, &username, &name, &authorPublic, &bookID, &bookTitle, &bookCover)
		if err != nil {
			return out, notFoundAs(err, ErrAttachmentUnavailable)
		}
		if isPrivate || (author != sender && !authorPublic) {
			return out, ErrAttachmentUnavailable
		}
		out.refID = id.String()
		m := map[string]any{"excerpt": truncateRunes(strings.TrimSpace(content), thoughtExcerptLen),
			"author_username": username, "author_name": name, "rating": rating,
			"book_title": deref(bookTitle), "book_cover_url": deref(bookCover), "book_id": ""}
		if bookID != nil {
			m["book_id"] = bookID.String()
		}
		snap = m

	case AttachProfile:
		var (
			id                        uuid.UUID
			username, name, avatarURL string
			isPublic                  bool
		)
		q := `SELECT id, username, COALESCE(name, ''), COALESCE(avatar_url, ''), is_public FROM users
		      WHERE deleted_at IS NULL AND `
		var err error
		if uid, perr := uuid.Parse(refID); perr == nil {
			err = s.pool.QueryRow(ctx, q+`id = $1`, uid).Scan(&id, &username, &name, &avatarURL, &isPublic)
		} else {
			err = s.pool.QueryRow(ctx, q+`username = $1`, strings.ToLower(strings.TrimPrefix(refID, "@"))).
				Scan(&id, &username, &name, &avatarURL, &isPublic)
		}
		if err != nil {
			return out, notFoundAs(err, ErrAttachmentUnavailable)
		}
		var blocked bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM blocks
			               WHERE (blocker_id = $1 AND blocked_id = $2) OR (blocker_id = $2 AND blocked_id = $1))
		`, sender, id).Scan(&blocked); err != nil {
			return out, err
		}
		if blocked {
			return out, ErrAttachmentUnavailable
		}
		out.refID = id.String()
		snap = map[string]any{"username": username, "name": name, "avatar_url": avatarURL, "is_public": isPublic}

	case AttachFusion:
		id, err := uuid.Parse(refID)
		if err != nil {
			return out, ErrAttachmentUnavailable
		}
		var (
			ua, ub                uuid.UUID
			score                 *int
			aUser, aName, aAvatar string
			bUser, bName, bAvatar string
		)
		err = s.pool.QueryRow(ctx, `
			SELECT f.user_a, f.user_b, (f.snapshot->'a'->>'score')::int,
			       ua.username, COALESCE(ua.name, ''), COALESCE(ua.avatar_url, ''),
			       ub.username, COALESCE(ub.name, ''), COALESCE(ub.avatar_url, '')
			FROM fusions f
			JOIN users ua ON ua.id = f.user_a AND ua.deleted_at IS NULL
			JOIN users ub ON ub.id = f.user_b AND ub.deleted_at IS NULL
			WHERE f.id = $1
		`, id).Scan(&ua, &ub, &score, &aUser, &aName, &aAvatar, &bUser, &bName, &bAvatar)
		if err != nil {
			return out, notFoundAs(err, ErrAttachmentUnavailable)
		}
		if sender != ua && sender != ub {
			return out, ErrAttachmentUnavailable
		}
		out.refID = id.String()
		snap = map[string]any{"score": score, "readers": []map[string]string{
			{"username": aUser, "name": aName, "avatar_url": aAvatar},
			{"username": bUser, "name": bName, "avatar_url": bAvatar},
		}}
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		return out, err
	}
	out.snapshot = raw
	return out, nil
}

func notFoundAs(err, as error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return as
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ── Reads ────────────────────────────────────────────────────────────────────

// ListConversations returns the inbox ("accepted") or Requests ("request"),
// most recent first. before is the previous page's next_cursor.
func (s *MessageService) ListConversations(ctx context.Context, userID, box, before string) (ConversationList, error) {
	out := ConversationList{Conversations: []ConversationSummary{}}
	if box != ConvRequest {
		box = ConvAccepted
	}
	var cursor *time.Time
	if before != "" {
		t, err := time.Parse(time.RFC3339Nano, before)
		if err != nil {
			return out, fmt.Errorf("%w: bad cursor", ErrInvalidMessage)
		}
		cursor = &t
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text, me.state, me.muted, c.last_message_at,
		       u.id::text, u.username, COALESCE(u.name, ''), COALESCE(u.avatar_url, ''),
		       m.sender_id = $1, m.body, m.deleted_at IS NOT NULL,
		       COALESCE((SELECT a.kind FROM message_attachments a WHERE a.message_id = m.id
		                 ORDER BY a.position LIMIT 1), ''),
		       (SELECT COUNT(*) FROM (
		            SELECT 1 FROM messages x
		            WHERE x.conversation_id = c.id AND x.id > me.last_read_message_id
		              AND x.sender_id <> $1 AND x.deleted_at IS NULL
		              AND (me.cleared_at IS NULL OR x.created_at > me.cleared_at)
		            LIMIT 99) unread)
		FROM conversation_members me
		JOIN conversations c ON c.id = me.conversation_id
		JOIN users u ON u.id = CASE WHEN c.user_a = $1 THEN c.user_b ELSE c.user_a END
		            AND u.deleted_at IS NULL
		JOIN messages m ON m.id = c.last_message_id
		WHERE me.user_id = $1 AND me.state = $2
		  AND (me.cleared_at IS NULL OR c.last_message_at > me.cleared_at)
		  AND ($3::timestamptz IS NULL OR c.last_message_at < $3)
		  AND NOT EXISTS (
		      SELECT 1 FROM blocks bl
		      WHERE (bl.blocker_id = $1 AND bl.blocked_id = u.id)
		         OR (bl.blocker_id = u.id AND bl.blocked_id = $1))
		ORDER BY c.last_message_at DESC
		LIMIT $4
	`, userID, box, cursor, conversationPageSize)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var last time.Time
	for rows.Next() {
		var (
			c         ConversationSummary
			body      string
			unsent    bool
			firstKind string
		)
		if err := rows.Scan(&c.ID, &c.State, &c.Muted, &last,
			&c.Them.UserID, &c.Them.Username, &c.Them.Name, &c.Them.AvatarURL,
			&c.LastMessage.FromMe, &body, &unsent, &firstKind, &c.UnreadCount); err != nil {
			return out, err
		}
		c.LastMessage.CreatedAt = last
		c.LastMessage.Text = previewText(body, firstKind, unsent)
		out.Conversations = append(out.Conversations, c)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Conversations) == conversationPageSize {
		out.NextCursor = last.Format(time.RFC3339Nano)
	}
	return out, nil
}

// Badge is the header count: unread unmuted chats, plus pending requests.
func (s *MessageService) Badge(ctx context.Context, userID string) (InboxBadge, error) {
	var b InboxBadge
	err := s.pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE me.state = 'accepted' AND NOT me.muted
		                     AND c.last_message_id > me.last_read_message_id),
		  COUNT(*) FILTER (WHERE me.state = 'request')
		FROM conversation_members me
		JOIN conversations c ON c.id = me.conversation_id
		JOIN users u ON u.id = CASE WHEN c.user_a = $1 THEN c.user_b ELSE c.user_a END
		            AND u.deleted_at IS NULL
		WHERE me.user_id = $1
		  AND c.last_message_id IS NOT NULL
		  AND (me.cleared_at IS NULL OR c.last_message_at > me.cleared_at)
		  AND NOT EXISTS (
		      SELECT 1 FROM blocks bl
		      WHERE (bl.blocker_id = $1 AND bl.blocked_id = u.id)
		         OR (bl.blocker_id = u.id AND bl.blocked_id = $1))
	`, userID).Scan(&b.Unread, &b.Requests)
	return b, err
}

// memberView loads the caller's side of a conversation plus the other reader.
// Not a member, a block, or a deleted partner all read as not found.
func (s *MessageService) memberView(ctx context.Context, convID, userID string) (ConversationPage, *time.Time, error) {
	var (
		p       ConversationPage
		cleared *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT c.id::text, me.state, me.muted, me.cleared_at, them.last_read_message_id,
		       u.id::text, u.username, COALESCE(u.name, ''), COALESCE(u.avatar_url, '')
		FROM conversations c
		JOIN conversation_members me ON me.conversation_id = c.id AND me.user_id = $2
		JOIN conversation_members them ON them.conversation_id = c.id AND them.user_id <> $2
		JOIN users u ON u.id = them.user_id AND u.deleted_at IS NULL
		WHERE c.id = $1
		  AND NOT EXISTS (
		      SELECT 1 FROM blocks bl
		      WHERE (bl.blocker_id = $2 AND bl.blocked_id = u.id)
		         OR (bl.blocker_id = u.id AND bl.blocked_id = $2))
	`, convID, userID).Scan(&p.ID, &p.State, &p.Muted, &cleared, &p.TheirLastReadID,
		&p.Them.UserID, &p.Them.Username, &p.Them.Name, &p.Them.AvatarURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil, ErrConversationNotFound
	}
	return p, cleared, err
}

// Messages returns a page of a conversation, oldest first. With after it
// returns what arrived since (the polling path); with before, older history;
// with neither, the latest page.
func (s *MessageService) Messages(ctx context.Context, convID, userID string, before, after int64) (ConversationPage, error) {
	if _, err := uuid.Parse(convID); err != nil {
		return ConversationPage{}, ErrConversationNotFound
	}
	p, cleared, err := s.memberView(ctx, convID, userID)
	if err != nil {
		return p, err
	}
	p.Messages = []Message{}

	var rows pgx.Rows
	if after > 0 {
		rows, err = s.pool.Query(ctx, `
			SELECT id, sender_id::text, body, created_at, deleted_at IS NOT NULL
			FROM messages
			WHERE conversation_id = $1 AND id > $2 AND ($3::timestamptz IS NULL OR created_at > $3)
			ORDER BY id ASC LIMIT 200
		`, convID, after, cleared)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, sender_id::text, body, created_at, deleted_at IS NOT NULL
			FROM messages
			WHERE conversation_id = $1 AND ($2::bigint = 0 OR id < $2)
			  AND ($3::timestamptz IS NULL OR created_at > $3)
			ORDER BY id DESC LIMIT $4
		`, convID, before, cleared, messagePageSize+1)
	}
	if err != nil {
		return p, err
	}
	msgs, err := scanMessages(rows, convID, userID)
	if err != nil {
		return p, err
	}
	if after <= 0 {
		if len(msgs) > messagePageSize {
			p.HasMore = true
			msgs = msgs[:messagePageSize]
		}
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}
	if err := s.attachTo(ctx, msgs); err != nil {
		return p, err
	}
	p.Messages = msgs
	return p, nil
}

func scanMessages(rows pgx.Rows, convID, viewerID string) ([]Message, error) {
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m := Message{ConversationID: convID, Attachments: []MessageAttachment{}}
		if err := rows.Scan(&m.ID, &m.SenderID, &m.Body, &m.CreatedAt, &m.Unsent); err != nil {
			return nil, err
		}
		m.FromMe = m.SenderID == viewerID
		out = append(out, m)
	}
	return out, rows.Err()
}

// attachTo fills attachments for a page of messages in one query.
func (s *MessageService) attachTo(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]int64, len(msgs))
	idx := make(map[int64]int, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
		idx[m.ID] = i
	}
	rows, err := s.pool.Query(ctx, `
		SELECT message_id, kind, ref_id, snapshot FROM message_attachments
		WHERE message_id = ANY($1) ORDER BY message_id, position
	`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			mid int64
			a   MessageAttachment
		)
		if err := rows.Scan(&mid, &a.Kind, &a.RefID, &a.Snapshot); err != nil {
			return err
		}
		i := idx[mid]
		msgs[i].Attachments = append(msgs[i].Attachments, a)
	}
	return rows.Err()
}

func (s *MessageService) messageByID(ctx context.Context, id int64, viewer uuid.UUID) (Message, error) {
	var convID string
	if err := s.pool.QueryRow(ctx, `SELECT conversation_id::text FROM messages WHERE id = $1`, id).Scan(&convID); err != nil {
		return Message{}, notFoundAs(err, ErrMessageNotFound)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, sender_id::text, body, created_at, deleted_at IS NOT NULL FROM messages WHERE id = $1
	`, id)
	if err != nil {
		return Message{}, err
	}
	msgs, err := scanMessages(rows, convID, viewer.String())
	if err != nil || len(msgs) == 0 {
		return Message{}, notFoundAs(pgx.ErrNoRows, ErrMessageNotFound)
	}
	if err := s.attachTo(ctx, msgs); err != nil {
		return Message{}, err
	}
	return msgs[0], nil
}

// ConversationWith finds the caller's conversation with a reader, for opening
// a chat from a profile. An empty ID means none yet: the first send creates it.
func (s *MessageService) ConversationWith(ctx context.Context, userID, username string) (string, MessagePerson, error) {
	var (
		them    MessagePerson
		blocked bool
		convID  *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.id::text, u.username, COALESCE(u.name, ''), COALESCE(u.avatar_url, ''),
		       EXISTS (SELECT 1 FROM blocks bl
		               WHERE (bl.blocker_id = $1 AND bl.blocked_id = u.id)
		                  OR (bl.blocker_id = u.id AND bl.blocked_id = $1)),
		       (SELECT c.id::text FROM conversations c
		        JOIN conversation_members me ON me.conversation_id = c.id AND me.user_id = $1
		        WHERE (c.user_a = $1 AND c.user_b = u.id) OR (c.user_b = $1 AND c.user_a = u.id))
		FROM users u
		WHERE u.username = $2 AND u.deleted_at IS NULL
	`, userID, strings.ToLower(strings.TrimPrefix(username, "@"))).
		Scan(&them.UserID, &them.Username, &them.Name, &them.AvatarURL, &blocked, &convID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", them, ErrRecipientNotFound
	}
	if err != nil {
		return "", them, err
	}
	if blocked || them.UserID == userID {
		return "", them, ErrCannotMessage
	}
	if convID == nil {
		return "", them, nil
	}
	return *convID, them, nil
}

// Shared lists everything sent in a conversation, newest first, with a count
// per kind. kind filters; before is the previous page's next_cursor.
func (s *MessageService) Shared(ctx context.Context, convID, userID, kind string, before int64) (SharedList, error) {
	out := SharedList{Counts: map[string]int{}, Items: []SharedItem{}}
	if _, err := uuid.Parse(convID); err != nil {
		return out, ErrConversationNotFound
	}
	if kind != "" && !validAttachKind(kind) {
		return out, fmt.Errorf("%w: bad kind", ErrInvalidMessage)
	}
	_, cleared, err := s.memberView(ctx, convID, userID)
	if err != nil {
		return out, err
	}

	counts, err := s.pool.Query(ctx, `
		SELECT a.kind, COUNT(*) FROM message_attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE a.conversation_id = $1 AND m.deleted_at IS NULL
		  AND ($2::timestamptz IS NULL OR m.created_at > $2)
		GROUP BY a.kind
	`, convID, cleared)
	if err != nil {
		return out, err
	}
	for counts.Next() {
		var k string
		var n int
		if err := counts.Scan(&k, &n); err != nil {
			counts.Close()
			return out, err
		}
		out.Counts[k] = n
	}
	counts.Close()
	if err := counts.Err(); err != nil {
		return out, err
	}

	const pageSize = 60
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, m.id, a.kind, a.ref_id, a.snapshot, m.sender_id::text = $3, m.created_at
		FROM message_attachments a
		JOIN messages m ON m.id = a.message_id
		WHERE a.conversation_id = $1 AND m.deleted_at IS NULL
		  AND ($2::timestamptz IS NULL OR m.created_at > $2)
		  AND ($4 = '' OR a.kind = $4)
		  AND ($5::bigint = 0 OR a.id < $5)
		ORDER BY a.id DESC LIMIT $6
	`, convID, cleared, userID, kind, before, pageSize)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var it SharedItem
		if err := rows.Scan(&it.cursor, &it.MessageID, &it.Kind, &it.RefID, &it.Snapshot, &it.FromMe, &it.CreatedAt); err != nil {
			return out, err
		}
		out.Items = append(out.Items, it)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) == pageSize {
		out.NextCursor = strconv.FormatInt(out.Items[pageSize-1].cursor, 10)
	}
	return out, nil
}

// ── Member actions ───────────────────────────────────────────────────────────

// MarkRead moves the caller's read marker to the latest message.
func (s *MessageService) MarkRead(ctx context.Context, convID, userID string) error {
	return s.memberExec(ctx, `
		UPDATE conversation_members me
		SET last_read_message_id = GREATEST(me.last_read_message_id, COALESCE(c.last_message_id, 0))
		FROM conversations c
		WHERE c.id = me.conversation_id AND me.conversation_id = $1 AND me.user_id = $2
	`, convID, userID)
}

// Accept moves a request into the inbox.
func (s *MessageService) Accept(ctx context.Context, convID, userID string) error {
	return s.memberExec(ctx, `
		UPDATE conversation_members SET state = 'accepted'
		WHERE conversation_id = $1 AND user_id = $2
	`, convID, userID)
}

func (s *MessageService) SetMuted(ctx context.Context, convID, userID string, muted bool) error {
	return s.memberExec(ctx, `
		UPDATE conversation_members SET muted = $3
		WHERE conversation_id = $1 AND user_id = $2
	`, convID, userID, muted)
}

// Clear is "delete chat": hides the history for the caller only. A new
// message brings the conversation back, starting from that message.
func (s *MessageService) Clear(ctx context.Context, convID, userID string) error {
	return s.memberExec(ctx, `
		UPDATE conversation_members me
		SET cleared_at = NOW(), last_read_message_id = COALESCE(c.last_message_id, 0)
		FROM conversations c
		WHERE c.id = me.conversation_id AND me.conversation_id = $1 AND me.user_id = $2
	`, convID, userID)
}

func (s *MessageService) memberExec(ctx context.Context, sql, convID, userID string, args ...any) error {
	if _, err := uuid.Parse(convID); err != nil {
		return ErrConversationNotFound
	}
	tag, err := s.pool.Exec(ctx, sql, append([]any{convID, userID}, args...)...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// Unsend wipes the caller's own message; the row stays as a "Message unsent"
// marker so the other side's thread does not silently reflow.
func (s *MessageService) Unsend(ctx context.Context, messageID int64, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE messages SET body = '', deleted_at = NOW()
		WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL
	`, messageID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrMessageNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM message_attachments WHERE message_id = $1`, messageID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReportContext checks the reporter can see a message and returns a copy of it
// for the report, so unsending afterwards cannot erase the evidence. Reporting
// your own message is refused.
func (s *MessageService) ReportContext(ctx context.Context, messageID int64, reporterID string) (string, error) {
	var (
		sender, body string
		kinds        []string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.username, m.body,
		       COALESCE(ARRAY(SELECT a.kind || ':' || a.ref_id FROM message_attachments a
		                      WHERE a.message_id = m.id ORDER BY a.position), '{}')
		FROM messages m
		JOIN conversation_members me ON me.conversation_id = m.conversation_id AND me.user_id = $2
		JOIN users u ON u.id = m.sender_id
		WHERE m.id = $1 AND m.sender_id <> $2
	`, messageID, reporterID).Scan(&sender, &body, &kinds)
	if err != nil {
		return "", notFoundAs(err, ErrMessageNotFound)
	}
	snap := "[message from @" + sender + "] " + body
	if len(kinds) > 0 {
		snap += " [attachments: " + strings.Join(kinds, ", ") + "]"
	}
	return snap, nil
}
