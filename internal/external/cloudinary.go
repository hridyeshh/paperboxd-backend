package external

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CloudinaryClient performs signed uploads to Cloudinary using the stdlib only.
// It mirrors the transformation the Next.js web app applies (500x500 fill, face
// gravity) so avatars look identical across web and mobile.
type CloudinaryClient struct {
	cloudName  string
	apiKey     string
	apiSecret  string
	httpClient *http.Client
}

// NewCloudinaryClient returns a client, or nil if credentials are missing — the
// caller treats a nil client as "uploads disabled" and returns a 503.
func NewCloudinaryClient(cloudName, apiKey, apiSecret string) *CloudinaryClient {
	if cloudName == "" || apiKey == "" || apiSecret == "" {
		return nil
	}
	return &CloudinaryClient{
		cloudName:  cloudName,
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type cloudinaryUploadResponse struct {
	SecureURL string `json:"secure_url"`
	PublicID  string `json:"public_id"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// avatarTransform matches lib/cloudinary.ts: square fill, face-aware crop, auto
// quality + format. Applied as an eager-style incoming transformation so the
// stored asset is already normalised.
const avatarTransform = "c_fill,g_face,h_500,w_500,q_auto:good,f_auto"

// UploadAvatar uploads raw image bytes and returns the secure delivery URL.
// publicID is set to a stable per-user value with overwrite so re-uploads
// replace the previous avatar instead of accumulating orphans.
func (c *CloudinaryClient) UploadAvatar(ctx context.Context, userID string, image []byte, contentType string) (string, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	folder := "paperboxd/avatars"
	publicID := "user_" + userID

	// Params that must be signed (everything except file, api_key, resource_type,
	// cloud_name), sorted alphabetically and joined with the api_secret appended.
	signed := map[string]string{
		"folder":         folder,
		"overwrite":      "true",
		"public_id":      publicID,
		"timestamp":      timestamp,
		"transformation": avatarTransform,
	}
	signature := c.sign(signed)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for k, v := range signed {
		if err := writer.WriteField(k, v); err != nil {
			return "", fmt.Errorf("write field %s: %w", k, err)
		}
	}
	if err := writer.WriteField("api_key", c.apiKey); err != nil {
		return "", fmt.Errorf("write api_key: %w", err)
	}
	if err := writer.WriteField("signature", signature); err != nil {
		return "", fmt.Errorf("write signature: %w", err)
	}

	part, err := writer.CreateFormFile("file", "avatar")
	if err != nil {
		return "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := part.Write(image); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close writer: %w", err)
	}

	endpoint := fmt.Sprintf("https://api.cloudinary.com/v1_1/%s/image/upload", c.cloudName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call cloudinary: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var parsed cloudinaryUploadResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode cloudinary response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 || parsed.SecureURL == "" {
		msg := "unknown error"
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return "", fmt.Errorf("cloudinary upload failed (status %d): %s", resp.StatusCode, msg)
	}
	return parsed.SecureURL, nil
}

// sign builds the Cloudinary upload signature: SHA1 of "k=v&k=v"+apiSecret with
// keys sorted alphabetically.
func (c *CloudinaryClient) sign(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+params[k])
	}
	toSign := strings.Join(pairs, "&") + c.apiSecret

	sum := sha1.Sum([]byte(toSign))
	return hex.EncodeToString(sum[:])
}
