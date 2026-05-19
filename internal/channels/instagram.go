package channels

// Instagram Messaging provider (Meta's Instagram Messaging API).
//
// Key differences from the Facebook Messenger provider:
//
//   • Webhook payload has "object": "instagram" instead of "page".
//     The entry structure and messaging array shape are identical to Messenger.
//
//   • Signature verification uses the same HMAC-SHA256 with FB_APP_SECRET —
//     it's the same Meta app, so no extra secrets are needed.
//
//   • Handshake verification reuses the same FB_VERIFY_TOKEN.
//
//   • OAuth needs additional scopes:
//       instagram_basic, instagram_manage_messages
//     plus pages_show_list + pages_manage_metadata (to subscribe webhooks).
//
//   • After OAuth we resolve each Facebook page's linked Instagram Business
//     Account by calling GET /{page-id}?fields=instagram_business_account.
//
//   • Send API: POST /{ig-user-id}/messages?access_token={page_token}
//     where ig-user-id is the Instagram Business Account ID (IGID), not
//     the page id. The recipient is the customer's Instagram-Scoped User ID
//     (IGSID) from evt.ExternalUserID.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

type InstagramProvider struct{}

func NewInstagramProvider() *InstagramProvider { return &InstagramProvider{} }

func (InstagramProvider) Name() string { return models.ProviderInstagram }

// HandshakeVerify — same hub.challenge dance as Facebook (same app, same token).
func (InstagramProvider) HandshakeVerify(q map[string]string, cfg *config.Config) (bool, string) {
	if q["hub.mode"] == "subscribe" && cfg.FBVerifyToken != "" && q["hub.verify_token"] == cfg.FBVerifyToken {
		return true, q["hub.challenge"]
	}
	return false, ""
}

// ParseEvents extracts inbound Instagram DM events from the webhook body.
// Meta sends: { "object": "instagram", "entry": [{ "id": "<igid>",
// "messaging": [{ "sender": { "id": "<igsid>" }, ... }] }] }
func (InstagramProvider) ParseEvents(body []byte) ([]ParsedEvent, error) {
	var p struct {
		Object string `json:"object"`
		Entry  []struct {
			ID        string `json:"id"` // Instagram Business Account ID
			Time      int64  `json:"time"`
			Messaging []struct {
				Sender struct {
					ID string `json:"id"`
				} `json:"sender"`
				Recipient struct {
					ID string `json:"id"`
				} `json:"recipient"`
				Timestamp int64 `json:"timestamp"`
				Message   *struct {
					MID         string `json:"mid"`
					Text        string `json:"text"`
					IsEcho      bool   `json:"is_echo"`
					Attachments []struct {
						Type    string `json:"type"`
						Payload struct {
							URL string `json:"url"`
						} `json:"payload"`
					} `json:"attachments"`
				} `json:"message"`
			} `json:"messaging"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	// Only handle Instagram webhook objects.
	if p.Object != "instagram" {
		return nil, nil
	}

	out := []ParsedEvent{}
	for _, e := range p.Entry {
		for _, m := range e.Messaging {
			if m.Message == nil || m.Message.IsEcho {
				continue
			}
			text := strings.TrimSpace(m.Message.Text)
			attachments := make([]models.Attachment, 0, len(m.Message.Attachments))
			for _, a := range m.Message.Attachments {
				if a.Type == "image" && a.Payload.URL != "" {
					attachments = append(attachments, models.Attachment{
						Type: "image",
						URL:  a.Payload.URL,
					})
				}
			}
			if text == "" && len(attachments) == 0 {
				continue
			}
			ts := time.UnixMilli(m.Timestamp)
			if m.Timestamp == 0 {
				ts = time.UnixMilli(e.Time)
			}
			out = append(out, ParsedEvent{
				ExternalChannelID: e.ID, // IGID of the business account
				ExternalUserID:    m.Sender.ID,
				Text:              text,
				Attachments:       attachments,
				Timestamp:         ts,
			})
		}
	}
	return out, nil
}

// VerifySignature — identical to Facebook: HMAC-SHA256 over the raw body
// with FB_APP_SECRET, checked against X-Hub-Signature-256.
func (InstagramProvider) VerifySignature(headers map[string]string, body []byte, cfg *config.Config, _ *models.ChannelConnection) bool {
	if cfg.FBAppSecret == "" {
		return true
	}
	sig := headerCI(headers, "X-Hub-Signature-256")
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return false
	}
	want, err := hex.DecodeString(sig[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(cfg.FBAppSecret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// Send dispatches a text reply to the customer via the Instagram Messaging API.
// conn.Credentials must contain:
//
//	"page_access_token" — the page-level token for the linked FB page
//	"ig_user_id"        — the Instagram Business Account ID (IGID)
func (InstagramProvider) Send(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, reply string) error {
	token := conn.Credentials["page_access_token"]
	igUserID := conn.Credentials["ig_user_id"]
	if token == "" || igUserID == "" {
		return fmt.Errorf("instagram send: missing page_access_token or ig_user_id")
	}
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(igUserID) +
		"/messages?access_token=" + url.QueryEscape(token)
	body, _ := json.Marshal(map[string]any{
		"recipient":      map[string]string{"id": evt.ExternalUserID},
		"message":        map[string]string{"text": reply},
		"messaging_type": "RESPONSE",
	})
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("instagram send: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// SendImage sends an image reply via the Instagram Messaging API.
func (InstagramProvider) SendImage(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, imageURL string) error {
	token := conn.Credentials["page_access_token"]
	igUserID := conn.Credentials["ig_user_id"]
	if token == "" || igUserID == "" {
		return fmt.Errorf("instagram send image: missing page_access_token or ig_user_id")
	}
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(igUserID) +
		"/messages?access_token=" + url.QueryEscape(token)
	body, _ := json.Marshal(map[string]any{
		"recipient": map[string]string{"id": evt.ExternalUserID},
		"message": map[string]any{
			"attachment": map[string]any{
				"type":    "image",
				"payload": map[string]any{"url": imageURL, "is_reusable": true},
			},
		},
		"messaging_type": "RESPONSE",
	})
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("instagram send image: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// ── OAuth helpers ──────────────────────────────────────────────────────

// InstagramLoginURL returns the Meta OAuth URL with the scopes needed for
// Instagram Messaging. The callback URL is registered separately in the Meta
// app dashboard under "Instagram → Webhooks" and "Facebook Login → OAuth".
func InstagramLoginURL(cfg *config.Config, state string) string {
	q := url.Values{}
	q.Set("client_id", cfg.FBAppID)
	q.Set("redirect_uri", cfg.IGOAuthRedirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	q.Set("scope", "pages_show_list,pages_manage_metadata,instagram_basic,instagram_manage_messages")
	return "https://www.facebook.com/" + graphVersion + "/dialog/oauth?" + q.Encode()
}

// InstagramExchangeCode swaps an OAuth code for a long-lived user token.
// Reuses the same token-exchange logic as Facebook (same app).
func InstagramExchangeCode(ctx context.Context, cfg *config.Config, code string) (string, error) {
	short, err := fbGraphTokenExchange(ctx, url.Values{
		"client_id":     {cfg.FBAppID},
		"client_secret": {cfg.FBAppSecret},
		"redirect_uri":  {cfg.IGOAuthRedirectURI},
		"code":          {code},
	})
	if err != nil {
		return "", fmt.Errorf("ig oauth (short-lived): %w", err)
	}
	long, err := fbGraphTokenExchange(ctx, url.Values{
		"grant_type":        {"fb_exchange_token"},
		"client_id":         {cfg.FBAppID},
		"client_secret":     {cfg.FBAppSecret},
		"fb_exchange_token": {short},
	})
	if err != nil {
		return "", fmt.Errorf("ig oauth (long-lived): %w", err)
	}
	return long, nil
}

// InstagramListAccounts lists all Instagram Business Accounts the user can
// access by walking their Facebook pages and resolving the linked IG account.
func InstagramListAccounts(ctx context.Context, userAccessToken string) ([]models.InstagramOAuthAccount, error) {
	// Step 1: list pages (same as FB flow).
	pages, err := FacebookListPages(ctx, userAccessToken)
	if err != nil {
		return nil, fmt.Errorf("ig list accounts: list pages: %w", err)
	}

	var out []models.InstagramOAuthAccount
	for _, page := range pages {
		// Step 2: for each page, look up the linked Instagram Business Account.
		u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(page.ID) +
			"?fields=instagram_business_account{id,name,username}&access_token=" +
			url.QueryEscape(page.AccessToken)
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		var pageResp struct {
			IBA *struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Username string `json:"username"`
			} `json:"instagram_business_account"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&pageResp)
		resp.Body.Close()

		if pageResp.IBA == nil || pageResp.IBA.ID == "" {
			// This page has no linked Instagram Business Account.
			continue
		}
		name := pageResp.IBA.Name
		if name == "" {
			name = pageResp.IBA.Username
		}
		if name == "" {
			name = page.Name + " (Instagram)"
		}
		out = append(out, models.InstagramOAuthAccount{
			IGID:            pageResp.IBA.ID,
			Name:            name,
			Username:        pageResp.IBA.Username,
			PageID:          page.ID,
			PageAccessToken: page.AccessToken,
		})
	}
	return out, nil
}

// InstagramSubscribeAccount subscribes the Meta app to Instagram DM webhooks
// for the given Instagram Business Account ID. Requires pages_manage_metadata.
// Idempotent — safe to call on an already-subscribed account.
func InstagramSubscribeAccount(ctx context.Context, pageAccessToken, igUserID string) error {
	if pageAccessToken == "" {
		return fmt.Errorf("instagram subscribe: page access token is empty")
	}
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(igUserID) + "/subscribed_apps"
	body := url.Values{
		"subscribed_fields": {"messages,messaging_postbacks"},
		"access_token":      {pageAccessToken},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewBufferString(body.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("instagram subscribe: %s: %s", resp.Status, string(rb))
	}
	return nil
}
