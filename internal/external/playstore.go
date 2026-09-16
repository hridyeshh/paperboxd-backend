package external

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Google Play Developer API, the two calls Plus needs: read a subscription by
// purchase token and acknowledge it. Auth is a service account: sign a JWT
// with its key, trade it for a bearer token, cache until expiry. Done with
// the JWT library already in go.mod rather than pulling in google.golang.org.
type PlayStoreClient struct {
	packageName string
	clientEmail string
	privateKey  []byte
	tokenURI    string
	httpClient  *http.Client
	baseURL     string

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// PlaySubscription is the subset of purchases.subscriptionsv2 we act on.
type PlaySubscription struct {
	SubscriptionState    string    `json:"subscriptionState"`
	AcknowledgementState string    `json:"acknowledgementState"`
	LinkedPurchaseToken  string    `json:"linkedPurchaseToken"`
	TestPurchase         *struct{} `json:"testPurchase"`
	LineItems            []struct {
		ProductID        string    `json:"productId"`
		ExpiryTime       time.Time `json:"expiryTime"`
		AutoRenewingPlan *struct {
			AutoRenewEnabled bool `json:"autoRenewEnabled"`
		} `json:"autoRenewingPlan"`
		OfferDetails *struct {
			OfferID string `json:"offerId"`
		} `json:"offerDetails"`
	} `json:"lineItems"`
}

// IsTrial: the only offer Plus configures is the free trial, so any offer id
// means a trial period. ponytail: read offerTags if a second offer ever ships.
func (s PlaySubscription) IsTrial() bool {
	for _, li := range s.LineItems {
		if li.OfferDetails != nil && li.OfferDetails.OfferID != "" {
			return true
		}
	}
	return false
}

// ProductID of the first line item; Plus sells one product with two base
// plans, so there is never more than one.
func (s PlaySubscription) ProductID() string {
	if len(s.LineItems) == 0 {
		return ""
	}
	return s.LineItems[0].ProductID
}

// ExpiresAt is the latest line-item expiry. Google folds refunds, holds and
// pauses into this date, so it alone decides the entitlement.
func (s PlaySubscription) ExpiresAt() time.Time {
	var latest time.Time
	for _, li := range s.LineItems {
		if li.ExpiryTime.After(latest) {
			latest = li.ExpiryTime
		}
	}
	return latest
}

func (s PlaySubscription) AutoRenew() bool {
	for _, li := range s.LineItems {
		if li.AutoRenewingPlan != nil && li.AutoRenewingPlan.AutoRenewEnabled {
			return true
		}
	}
	return false
}

func (s PlaySubscription) Environment() string {
	if s.TestPurchase != nil {
		return "sandbox"
	}
	return "production"
}

// NewPlayStoreClient takes the service account JSON as downloaded from Google
// Cloud. Returns nil when it is not configured so callers can 503 cleanly.
func NewPlayStoreClient(packageName, serviceAccountJSON string) *PlayStoreClient {
	if serviceAccountJSON == "" {
		return nil
	}
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(serviceAccountJSON), &sa); err != nil || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &PlayStoreClient{
		packageName: packageName,
		clientEmail: sa.ClientEmail,
		privateKey:  []byte(sa.PrivateKey),
		tokenURI:    sa.TokenURI,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
		baseURL:     "https://androidpublisher.googleapis.com/androidpublisher/v3",
	}
}

// GetSubscription reads purchases.subscriptionsv2 for a purchase token.
func (c *PlayStoreClient) GetSubscription(ctx context.Context, purchaseToken string) (PlaySubscription, error) {
	var sub PlaySubscription
	path := fmt.Sprintf("/applications/%s/purchases/subscriptionsv2/tokens/%s",
		url.PathEscape(c.packageName), url.PathEscape(purchaseToken))
	if err := c.do(ctx, http.MethodGet, path, nil, &sub); err != nil {
		return sub, err
	}
	if len(sub.LineItems) == 0 {
		return sub, errors.New("play: subscription has no line items")
	}
	return sub, nil
}

// Acknowledge tells Google we granted the entitlement. Unacknowledged
// subscriptions are refunded after three days, so this is not optional.
func (c *PlayStoreClient) Acknowledge(ctx context.Context, productID, purchaseToken string) error {
	path := fmt.Sprintf("/applications/%s/purchases/subscriptions/%s/tokens/%s:acknowledge",
		url.PathEscape(c.packageName), url.PathEscape(productID), url.PathEscape(purchaseToken))
	return c.do(ctx, http.MethodPost, path, []byte("{}"), nil)
}

func (c *PlayStoreClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	token, err := c.bearer(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("play: %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// bearer returns a cached access token, minting one when missing or within a
// minute of expiry.
func (c *PlayStoreClient) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Until(c.tokenExpiry) > time.Minute {
		return c.accessToken, nil
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(c.privateKey)
	if err != nil {
		return "", fmt.Errorf("play: service account key: %w", err)
	}
	now := time.Now()
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   c.clientEmail,
		"scope": "https://www.googleapis.com/auth/androidpublisher",
		"aud":   c.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}).SignedString(key)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("play: token exchange failed (%d)", resp.StatusCode)
	}
	c.accessToken = tok.AccessToken
	c.tokenExpiry = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// PlayNotification is a Real-time Developer Notification as delivered by a
// Pub/Sub push subscription: the interesting part is base64 JSON in
// message.data. We only need the purchase token — the state is re-read from
// Google rather than trusted from the notification.
type PlayNotification struct {
	Message struct {
		Data string `json:"data"`
	} `json:"message"`
}

// Play RTDN subscription notification types we map to analytics events.
const (
	PlayNotifyRenewed   = 2
	PlayNotifyCanceled  = 3
	PlayNotifyPurchased = 4
	PlayNotifyRestarted = 7
	PlayNotifyRevoked   = 12
	PlayNotifyExpired   = 13
)

// PurchaseToken extracts the token and notification type from a subscription
// or voided-purchase notification. Empty token for test notifications and
// anything else; voided purchases report as PlayNotifyRevoked.
func (n PlayNotification) PurchaseToken() (token string, notifyType int, err error) {
	raw, err := base64.StdEncoding.DecodeString(n.Message.Data)
	if err != nil {
		return "", 0, err
	}
	var body struct {
		SubscriptionNotification *struct {
			NotificationType int    `json:"notificationType"`
			PurchaseToken    string `json:"purchaseToken"`
		} `json:"subscriptionNotification"`
		VoidedPurchaseNotification *struct {
			PurchaseToken string `json:"purchaseToken"`
		} `json:"voidedPurchaseNotification"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", 0, err
	}
	switch {
	case body.SubscriptionNotification != nil:
		return body.SubscriptionNotification.PurchaseToken, body.SubscriptionNotification.NotificationType, nil
	case body.VoidedPurchaseNotification != nil:
		return body.VoidedPurchaseNotification.PurchaseToken, PlayNotifyRevoked, nil
	}
	return "", 0, nil
}
