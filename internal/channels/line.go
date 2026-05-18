package channels

// LINE Messaging API provider.
//
// Differences from Facebook worth knowing:
//
//   • Signature is HMAC-SHA256 with the *channel secret* (per-tenant), not an
//     app-level secret. So we have to look up the connection before we can
//     verify — but we already need the connection to know which tenant the
//     event belongs to, so it's no extra work.
//
//   • Each event carries a `replyToken` valid for ~30 seconds. Using it via
//     the Reply API is free and unmetered; the Push API counts against the
//     monthly free tier. We always prefer reply, and only push if the token
//     went stale (e.g. the orchestrator took too long).
//
//   • Connection happens by hand: the user pastes Channel ID, Channel Secret
//     and Channel Access Token from the LINE Developers console. There's no
//     OAuth flow.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/models"
)

type LineProvider struct{}

func NewLineProvider() *LineProvider { return &LineProvider{} }

func (LineProvider) Name() string { return models.ProviderLine }

// LINE doesn't use a GET handshake; the webhook URL is just verified by
// pressing "Verify" in the console which sends a POST with empty events.
func (LineProvider) HandshakeVerify(_ map[string]string, _ *config.Config) (bool, string) {
	return false, ""
}

func (LineProvider) ParseEvents(body []byte) ([]ParsedEvent, error) {
	var p struct {
		// `destination` is the bot user id (a "U…" string) of the channel
		// that received the event. The LINE console exposes this as the
		// "User ID" in the Messaging API tab — that's what tenants paste
		// into the Channel ID field when connecting.
		Destination string `json:"destination"`
		Events      []struct {
			Type       string `json:"type"`
			ReplyToken string `json:"replyToken"`
			Timestamp  int64  `json:"timestamp"`
			Source     struct {
				Type   string `json:"type"`
				UserID string `json:"userId"`
			} `json:"source"`
			Message *struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"message"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	out := make([]ParsedEvent, 0, len(p.Events))
	for _, e := range p.Events {
		if e.Type != "message" || e.Message == nil {
			continue
		}
		text := strings.TrimSpace(e.Message.Text)
		attachments := []models.Attachment{}
		if e.Message.Type == "image" && e.Message.ID != "" {
			attachments = append(attachments, models.Attachment{
				ID:          e.Message.ID,
				Type:        "image",
				URL:         "/api/v1/inbox/media/" + url.PathEscape(e.Message.ID),
				ContentType: "image/jpeg",
			})
		}
		if text == "" && len(attachments) == 0 {
			continue
		}
		out = append(out, ParsedEvent{
			ExternalChannelID: p.Destination,
			ExternalUserID:    e.Source.UserID,
			Text:              text,
			Attachments:       attachments,
			Timestamp:         time.UnixMilli(e.Timestamp),
			ReplyToken:        e.ReplyToken,
		})
	}
	return out, nil
}

// VerifySignature checks `X-Line-Signature` — base64(HMAC-SHA256(secret, body)).
//
// The secret is the connection's `channel_secret`, not anything in env.
// If we have no connection yet (signature checked before lookup) or no
// secret, we refuse — better to drop unverified traffic than risk replaying
// arbitrary events into the orchestrator.
func (LineProvider) VerifySignature(headers map[string]string, body []byte, _ *config.Config, conn *models.ChannelConnection) bool {
	if conn == nil {
		return false
	}
	secret := conn.Credentials["channel_secret"]
	if secret == "" {
		return false
	}
	sig := headerCI(headers, "X-Line-Signature")
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

func (LineProvider) Send(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, reply string) error {
	// Send only — token refresh happens in EnsureCredentials, called by the
	// generic webhook router before Send. If the cached token is still empty
	// here, it means refresh failed; bail loudly so the router can mark the
	// connection in-error.
	token := conn.Credentials["channel_access_token"]
	if token == "" {
		return fmt.Errorf("line send: no access token (refresh failed)")
	}
	// Prefer the (free, scoped) reply API. Fall back to push when the reply
	// token is unavailable — e.g. the orchestrator took longer than the
	// 30-second reply window.
	if evt.ReplyToken != "" {
		return lineReply(ctx, token, evt.ReplyToken, reply)
	}
	return linePush(ctx, token, evt.ExternalUserID, reply)
}

// EnsureCredentials implements CredentialRefresher. The router calls this
// before Send so the LINE provider can rotate its access token using the
// stored channel_id + channel_secret. Returns refreshed=true when conn was
// mutated (so the router can persist the change).
//
// Token lifetime is 30 days; we refresh proactively when within 24 h of
// expiry. The endpoint is `POST /v2/oauth/accessToken` with grant_type=
// client_credentials — works on every Messaging API channel without any
// extra setup by the customer.
func (LineProvider) EnsureCredentials(ctx context.Context, conn *models.ChannelConnection) (bool, error) {
	if conn == nil {
		return false, fmt.Errorf("nil conn")
	}
	if conn.Credentials == nil {
		conn.Credentials = map[string]string{}
	}
	tok := conn.Credentials["channel_access_token"]
	expStr := conn.Credentials["channel_access_token_expires_at"]
	if tok != "" && expStr != "" {
		if exp, err := time.Parse(time.RFC3339, expStr); err == nil {
			if time.Until(exp) > 24*time.Hour {
				return false, nil
			}
		}
	}
	cid := conn.Credentials["channel_id"]
	if cid == "" {
		cid = conn.ExternalID
	}
	sec := conn.Credentials["channel_secret"]
	if cid == "" || sec == "" {
		return false, fmt.Errorf("line refresh: missing channel_id / channel_secret")
	}
	fresh, expiresIn, err := LineIssueAccessToken(ctx, cid, sec)
	if err != nil {
		return false, err
	}
	conn.Credentials["channel_access_token"] = fresh
	if expiresIn > 0 {
		conn.Credentials["channel_access_token_expires_at"] =
			time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return true, nil
}

func lineReply(ctx context.Context, token, replyToken, text string) error {
	body, _ := json.Marshal(map[string]any{
		"replyToken": replyToken,
		"messages":   []map[string]string{{"type": "text", "text": text}},
	})
	return doLinePost(ctx, "https://api.line.me/v2/bot/message/reply", token, body)
}

func linePush(ctx context.Context, token, userID, text string) error {
	body, _ := json.Marshal(map[string]any{
		"to":       userID,
		"messages": []map[string]string{{"type": "text", "text": text}},
	})
	return doLinePost(ctx, "https://api.line.me/v2/bot/message/push", token, body)
}

// SendImage implements channels.ImageSender. It always uses the push API
// (images have no reply-token equivalent in LINE).
func (LineProvider) SendImage(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, imageURL string) error {
	token := conn.Credentials["channel_access_token"]
	if token == "" {
		return fmt.Errorf("line send image: no access token (refresh failed)")
	}
	return linePushImage(ctx, token, evt.ExternalUserID, imageURL)
}

func linePushImage(ctx context.Context, token, userID, imageURL string) error {
	body, _ := json.Marshal(map[string]any{
		"to": userID,
		"messages": []map[string]any{{
			"type":               "image",
			"originalContentUrl": imageURL,
			"previewImageUrl":    imageURL,
		}},
	})
	return doLinePost(ctx, "https://api.line.me/v2/bot/message/push", token, body)
}

func doLinePost(ctx context.Context, url, token string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("line: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// LineIssueAccessToken issues a long-lived (~30 day) channel access token
// from a channel id + secret. This is the magic that lets us avoid asking
// users to copy-paste the access token by hand: every Messaging API channel
// can mint tokens this way without any extra console setup.
//
//	POST https://api.line.me/v2/oauth/accessToken
//	grant_type=client_credentials & client_id=<id> & client_secret=<secret>
//
// Returns (token, expires_in_seconds, err). Up to two tokens issued this
// way can be live at the same time, so refresh-before-expiry is safe.
func LineIssueAccessToken(ctx context.Context, channelID, channelSecret string) (string, int, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {channelID},
		"client_secret": {channelSecret},
	}
	req, err := http.NewRequestWithContext(
		ctx, "POST",
		"https://api.line.me/v2/oauth/accessToken",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		// Most common case: typo in channel id or secret. Surface a clean
		// error string the dashboard can show verbatim.
		return "", 0, fmt.Errorf("invalid channel id or channel secret")
	}
	if resp.StatusCode >= 400 {
		return "", 0, fmt.Errorf("line oauth: %s: %s", resp.Status, string(rb))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", 0, err
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("line oauth: empty access_token in response")
	}
	return out.AccessToken, out.ExpiresIn, nil
}

// LineUserProfile fetches a customer's display name (and picture) so the
// inbox can show "สมชาย" instead of "LINE User abcd12". Requires that the
// user has added the bot as a friend AND not blocked profile sharing —
// returns 404 from LINE otherwise, in which case we fall back to the
// placeholder. Best-effort, called fire-and-forget on inbound messages.
//
//	GET /v2/bot/profile/{userId}
//
// Quota: 2,000 calls/sec — far more than we'll ever hit. Persist the
// result in `customer_profiles` so we don't re-fetch on every poll.
type LineUserProfileResp struct {
	UserID        string `json:"userId"`
	DisplayName   string `json:"displayName"`
	PictureURL    string `json:"pictureUrl"`
	StatusMessage string `json:"statusMessage"`
	Language      string `json:"language"`
}

func LineUserProfile(ctx context.Context, accessToken, userID string) (*LineUserProfileResp, error) {
	if accessToken == "" || userID == "" {
		return nil, fmt.Errorf("line profile: missing token or userId")
	}
	req, err := http.NewRequestWithContext(
		ctx, "GET",
		"https://api.line.me/v2/bot/profile/"+url.PathEscape(userID),
		nil,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// User isn't a friend / blocked profile sharing. Don't treat as
		// an error — caller will fall back to the placeholder name.
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("line profile: %s: %s", resp.Status, string(rb))
	}
	var out LineUserProfileResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LineBotInfo asks LINE who-am-I — used to fetch the bot's display name
// after we've issued a token so the dashboard can show "Connected: <name>"
// instead of just a numeric channel id.
//
// Best-effort: callers should treat a failure here as informational, not
// fatal — the connection is usable as long as token issuance succeeded.
func LineBotInfo(ctx context.Context, accessToken string) (botUserID, displayName string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.line.me/v2/bot/info", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("line bot/info: %s: %s", resp.Status, string(rb))
	}
	var out struct {
		UserID      string `json:"userId"`
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.UserID, out.DisplayName, nil
}

// headerCI looks up a header by name, falling back to the lowercase form.
// Fiber's `c.GetReqHeaders()` returns the canonical case but we also accept
// raw maps from tests.
func headerCI(h map[string]string, name string) string {
	if v, ok := h[name]; ok && v != "" {
		return v
	}
	return h[strings.ToLower(name)]
}
