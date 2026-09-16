package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hridyesh/paperboxd-backend/internal/config"
	"github.com/hridyesh/paperboxd-backend/internal/db"
	"github.com/hridyesh/paperboxd-backend/internal/external"
	"github.com/hridyesh/paperboxd-backend/internal/service"
	"github.com/hridyesh/paperboxd-backend/internal/types"
)

// SubscriptionHandler links store receipts to accounts and keeps them
// current from store webhooks. The entitlement itself is one column:
// subscriptions.expires_at > now().
type SubscriptionHandler struct {
	Pool     *pgxpool.Pool
	Queries  *db.Queries
	Config   *config.Config
	Apple    *external.AppleVerifier
	Play     *external.PlayStoreClient // nil when GOOGLE_PLAY_SERVICE_ACCOUNT_JSON is unset
	EventSvc *service.EventService
}

func NewSubscriptionHandler(pool *pgxpool.Pool, queries *db.Queries, cfg *config.Config, apple *external.AppleVerifier, play *external.PlayStoreClient, events *service.EventService) *SubscriptionHandler {
	return &SubscriptionHandler{Pool: pool, Queries: queries, Config: cfg, Apple: apple, Play: play, EventSvc: events}
}

// PlanStatus is what every client renders from: one bool, a capability map
// so no client hardcodes "active means X", and the Scan & Know allowance
// since it is the one free feature Plus changes the limit of.
type PlanStatus struct {
	Active         bool            `json:"active"`
	Entitlements   map[string]bool `json:"entitlements"`
	Store          string          `json:"store,omitempty"`
	ProductID      string          `json:"product_id,omitempty"`
	ExpiresAt      *time.Time      `json:"expires_at,omitempty"`
	AutoRenew      bool            `json:"auto_renew"`
	ScansRemaining int32           `json:"scans_remaining"`
	ScansUnlimited bool            `json:"scans_unlimited"`
}

// Everything Plus unlocks, all keyed on the one subscription. Split the map
// when a second tier exists, not before.
func entitlements(active bool) map[string]bool {
	return map[string]bool{
		"jazy":             active,
		"scan_unlimited":   active,
		"vibe_search":      active,
		"reading_insights": active,
		"taste_twins":      active,
	}
}

func statusOf(sub db.Subscription) PlanStatus {
	exp := sub.ExpiresAt.Time
	active := exp.After(time.Now())
	return PlanStatus{
		Active:       active,
		Entitlements: entitlements(active),
		Store:        sub.Store,
		ProductID:    sub.ProductID,
		ExpiresAt:    &exp,
		AutoRenew:    sub.AutoRenew,
	}
}

// subscriptionActive is the gate paid features call. Any error reads as not
// subscribed: a paid Claude call must never go out on a database hiccup.
func subscriptionActive(ctx context.Context, q *db.Queries, userID uuid.UUID) bool {
	sub, err := q.GetSubscription(ctx, userID)
	if err != nil {
		return false
	}
	return sub.ExpiresAt.Time.After(time.Now())
}

// requirePlus writes 402 and returns false when the caller has no active
// subscription. Handlers call it first so no paid work starts.
func requirePlus(w http.ResponseWriter, r *http.Request, q *db.Queries, userID uuid.UUID, feature string) bool {
	if subscriptionActive(r.Context(), q, userID) {
		return true
	}
	types.WriteError(w, http.StatusPaymentRequired, types.ErrCodeSubscriptionRequired, feature+" needs PaperBoxd Plus")
	return false
}

// Me returns the caller's plan status.
// GET /api/v1/subscriptions/me
func (h *SubscriptionHandler) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}
	status := PlanStatus{Entitlements: entitlements(false)}
	sub, err := h.Queries.GetSubscription(r.Context(), userID)
	switch {
	case err == nil:
		status = statusOf(sub)
	case !errors.Is(err, pgx.ErrNoRows):
		types.WriteInternalError(w)
		return
	}
	if remaining, unlimited, err := scanQuota(r.Context(), h.Pool, h.Queries, h.Config, userID); err == nil {
		status.ScansRemaining, status.ScansUnlimited = remaining, unlimited
	}
	types.WriteJSON(w, http.StatusOK, status)
}

// LinkApple verifies a StoreKit 2 signed transaction and attaches it to the
// caller. Idempotent: the client sends it on purchase, on restore, and from
// Transaction.updates, and every call lands on the same row.
// POST /api/v1/subscriptions/apple  {"jws": "<Transaction.jwsRepresentation>"}
func (h *SubscriptionHandler) LinkApple(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}
	var req struct {
		JWS string `json:"jws"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.JWS == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "jws is required")
		return
	}
	var tx external.AppleTransaction
	if err := h.Apple.Verify(req.JWS, &tx); err != nil {
		slog.Warn("apple receipt rejected", "error", err, "user", userID)
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Receipt could not be verified")
		return
	}
	if !slices.Contains(h.Config.AllowedAppleAudiences, tx.BundleID) {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Receipt is for another app")
		return
	}
	if tx.Type != "Auto-Renewable Subscription" || tx.ExpiresDate == 0 {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Not a subscription")
		return
	}
	h.upsert(w, r, db.UpsertSubscriptionParams{
		UserID:      userID,
		Store:       "apple",
		StoreID:     tx.OriginalTransactionID,
		ProductID:   tx.ProductID,
		ExpiresAt:   tsz(tx.ExpiresAt()),
		AutoRenew:   tx.RevocationDate == 0,
		Environment: envName(tx.Environment),
	}, tx.IsTrial())
}

// LinkGoogle verifies a Play Billing purchase token with Google, acknowledges
// it, and attaches it to the caller.
// POST /api/v1/subscriptions/google  {"purchase_token": "..."}
func (h *SubscriptionHandler) LinkGoogle(w http.ResponseWriter, r *http.Request) {
	userID, ok := authenticatedUserID(w, r)
	if !ok {
		return
	}
	if h.Play == nil {
		types.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Google Play billing is not configured")
		return
	}
	var req struct {
		PurchaseToken string `json:"purchase_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.PurchaseToken == "" {
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "purchase_token is required")
		return
	}
	sub, err := h.Play.GetSubscription(r.Context(), req.PurchaseToken)
	if err != nil {
		slog.Warn("play purchase rejected", "error", err, "user", userID)
		types.WriteError(w, http.StatusBadRequest, types.ErrCodeValidation, "Purchase could not be verified")
		return
	}
	if sub.AcknowledgementState == "ACKNOWLEDGEMENT_STATE_PENDING" {
		if err := h.Play.Acknowledge(r.Context(), sub.ProductID(), req.PurchaseToken); err != nil {
			// Not fatal for the reader — Google keeps the purchase for three
			// days and the client acknowledges too. Log so it is not silent.
			slog.Error("play acknowledge", "error", err, "user", userID)
		}
	}
	h.upsert(w, r, db.UpsertSubscriptionParams{
		UserID:      userID,
		Store:       "google",
		StoreID:     req.PurchaseToken,
		ProductID:   sub.ProductID(),
		ExpiresAt:   tsz(sub.ExpiresAt()),
		AutoRenew:   sub.AutoRenew(),
		Environment: sub.Environment(),
	}, sub.IsTrial())
}

func (h *SubscriptionHandler) upsert(w http.ResponseWriter, r *http.Request, params db.UpsertSubscriptionParams, trial bool) {
	// A row that already carries this receipt is a re-link (restore, renewal
	// listener), not a new subscription — no event for those.
	prev, prevErr := h.Queries.GetSubscription(r.Context(), params.UserID)
	fresh := errors.Is(prevErr, pgx.ErrNoRows) || (prevErr == nil && prev.StoreID != params.StoreID)

	sub, err := h.Queries.UpsertSubscription(r.Context(), params)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		// The (store, store_id) index: this receipt already backs another
		// account. Typical cause is a shared device with two sign-ins.
		types.WriteError(w, http.StatusConflict, types.ErrCodeConflict, "This subscription is linked to another account")
		return
	}
	if err != nil {
		slog.Error("subscription upsert", "error", err, "user", params.UserID)
		types.WriteInternalError(w)
		return
	}
	if fresh && sub.ExpiresAt.Time.After(time.Now()) {
		h.emit(sub.UserID, service.EventSubscriptionStarted, sub.Store, sub.ProductID, trial)
		if trial {
			h.emit(sub.UserID, service.EventTrialStarted, sub.Store, sub.ProductID, trial)
		}
	}
	types.WriteJSON(w, http.StatusOK, statusOf(sub))
}

// AppleWebhook takes App Store Server Notifications V2. Every notification
// carries the full current transaction, so the handling is the same for
// SUBSCRIBED, DID_RENEW, EXPIRED, REFUND and the rest: verify, then write
// the transaction's own expiry/revocation onto the row it belongs to.
// POST /api/v1/webhooks/apple
func (h *SubscriptionHandler) AppleWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SignedPayload string `json:"signedPayload"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil || req.SignedPayload == "" {
		http.Error(w, "signedPayload required", http.StatusBadRequest)
		return
	}
	var note external.AppleNotification
	if err := h.Apple.Verify(req.SignedPayload, &note); err != nil {
		slog.Warn("apple notification rejected", "error", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if note.Data.SignedTransactionInfo == "" {
		w.WriteHeader(http.StatusOK) // TEST or a type with no transaction
		return
	}
	var tx external.AppleTransaction
	if err := h.Apple.Verify(note.Data.SignedTransactionInfo, &tx); err != nil {
		slog.Warn("apple notification transaction rejected", "error", err)
		http.Error(w, "invalid transaction", http.StatusUnauthorized)
		return
	}
	autoRenew := tx.RevocationDate == 0
	if note.Data.SignedRenewalInfo != "" {
		var ri external.AppleRenewalInfo
		if err := h.Apple.Verify(note.Data.SignedRenewalInfo, &ri); err == nil {
			autoRenew = autoRenew && ri.AutoRenewStatus == 1
		}
	}
	userID, err := h.Queries.UpdateSubscriptionByStoreID(r.Context(), db.UpdateSubscriptionByStoreIDParams{
		Store:     "apple",
		StoreID:   tx.OriginalTransactionID,
		ProductID: tx.ProductID,
		ExpiresAt: tsz(tx.ExpiresAt()),
		AutoRenew: autoRenew,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("apple notification for unlinked receipt", "type", note.NotificationType, "original_transaction_id", tx.OriginalTransactionID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		slog.Error("apple notification update", "error", err)
		http.Error(w, "db", http.StatusInternalServerError) // Apple retries on 5xx
		return
	}
	var event string
	switch note.NotificationType {
	case "DID_RENEW":
		event = service.EventSubscriptionRenewed
	case "SUBSCRIBED":
		event = service.EventSubscriptionStarted
	case "EXPIRED", "REFUND", "REVOKE", "GRACE_PERIOD_EXPIRED":
		event = service.EventSubscriptionExpired
	case "DID_CHANGE_RENEWAL_STATUS":
		if note.Subtype == "AUTO_RENEW_DISABLED" {
			event = service.EventSubscriptionCancelled
		}
	}
	if event != "" {
		h.emit(userID, event, "apple", tx.ProductID, tx.IsTrial())
	}
	slog.Info("apple notification", "type", note.NotificationType, "subtype", note.Subtype, "user", userID)
	w.WriteHeader(http.StatusOK)
}

// GoogleWebhook takes Real-time Developer Notifications pushed by Pub/Sub.
// The notification is only a hint: the state is re-read from Google, which
// makes every notification type the same one update.
// POST /api/v1/webhooks/google?token=<GOOGLE_PLAY_RTDN_TOKEN>
func (h *SubscriptionHandler) GoogleWebhook(w http.ResponseWriter, r *http.Request) {
	if h.Play == nil || h.Config.GooglePlayRTDNToken == "" || r.URL.Query().Get("token") != h.Config.GooglePlayRTDNToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var note external.PlayNotification
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&note); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	token, notifyType, err := note.PurchaseToken()
	if err != nil {
		http.Error(w, "bad data", http.StatusBadRequest)
		return
	}
	if token == "" {
		w.WriteHeader(http.StatusOK) // test notification
		return
	}
	sub, err := h.Play.GetSubscription(r.Context(), token)
	if err != nil {
		slog.Error("play notification lookup", "error", err)
		http.Error(w, "lookup", http.StatusInternalServerError) // Pub/Sub retries on 5xx
		return
	}
	userID, err := h.Queries.UpdateSubscriptionByStoreID(r.Context(), db.UpdateSubscriptionByStoreIDParams{
		Store:     "google",
		StoreID:   token,
		ProductID: sub.ProductID(),
		ExpiresAt: tsz(sub.ExpiresAt()),
		AutoRenew: sub.AutoRenew(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("play notification for unlinked token", "type", notifyType)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		slog.Error("play notification update", "error", err)
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}
	var event string
	switch notifyType {
	case external.PlayNotifyRenewed:
		event = service.EventSubscriptionRenewed
	case external.PlayNotifyPurchased, external.PlayNotifyRestarted:
		event = service.EventSubscriptionStarted
	case external.PlayNotifyCanceled:
		event = service.EventSubscriptionCancelled
	case external.PlayNotifyExpired, external.PlayNotifyRevoked:
		event = service.EventSubscriptionExpired
	}
	if event != "" {
		h.emit(userID, event, "google", sub.ProductID(), sub.IsTrial())
	}
	slog.Info("play notification", "type", notifyType, "state", sub.SubscriptionState, "user", userID)
	w.WriteHeader(http.StatusOK)
}

// emit records a subscription lifecycle event. Fire and forget.
func (h *SubscriptionHandler) emit(userID uuid.UUID, eventType, store, productID string, trial bool) {
	if h.EventSvc == nil {
		return
	}
	go h.EventSvc.Emit(context.Background(), service.EmitParams{
		UserID:    userID,
		EventType: eventType,
		Source:    "server",
		Metadata:  map[string]any{"store": store, "product_id": productID, "trial": trial},
	})
}

func envName(appleEnv string) string {
	if appleEnv == "Sandbox" {
		return "sandbox"
	}
	return "production"
}
