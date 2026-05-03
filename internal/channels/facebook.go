package channels

// Facebook Messenger / Page provider.
//
// Differences from LINE worth knowing:
//
//   • Signature is HMAC-SHA256 with an app-level secret (FB_APP_SECRET in
//     env), shared across every page connected to this Meta app. So we
//     verify with `cfg`, not the connection.
//
//   • Connection happens via Facebook Login (OAuth). The user logs in,
//     grants `pages_messaging` + `pages_show_list`, we exchange the code
//     for a long-lived user token, list their manageable pages, and let
//     the user pick which pages to connect. Each page comes with its own
//     page-access-token which we store as the connection credential.
//
//   • Outbound replies use the Send API: POST /<page>/messages with
//     `recipient.id = sender_id`. There's no reply-token concept.

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

// graphVersion — bump when Meta deprecates the current. v20.0 is GA at
// time of writing and supports both Messenger Send API and Pages.
const graphVersion = "v20.0"

type FacebookProvider struct{}

func NewFacebookProvider() *FacebookProvider { return &FacebookProvider{} }

func (FacebookProvider) Name() string { return models.ProviderFacebook }

// HandshakeVerify — Meta's GET-based subscription verification. Console
// sends `?hub.mode=subscribe&hub.verify_token=<token>&hub.challenge=<c>`;
// echo back the challenge if the token matches FB_VERIFY_TOKEN.
func (FacebookProvider) HandshakeVerify(q map[string]string, cfg *config.Config) (bool, string) {
	if q["hub.mode"] == "subscribe" && cfg.FBVerifyToken != "" && q["hub.verify_token"] == cfg.FBVerifyToken {
		return true, q["hub.challenge"]
	}
	return false, ""
}

func (FacebookProvider) ParseEvents(body []byte) ([]ParsedEvent, error) {
	var p struct {
		Object string `json:"object"`
		Entry  []struct {
			ID        string `json:"id"` // page id
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
	out := []ParsedEvent{}
	for _, e := range p.Entry {
		for _, m := range e.Messaging {
			if m.Message == nil || m.Message.IsEcho {
				// `is_echo` events are our own outbound replies bouncing
				// back from Meta — drop them or we'll loop.
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
				ExternalChannelID: e.ID,
				ExternalUserID:    m.Sender.ID,
				Text:              text,
				Attachments:       attachments,
				Timestamp:         ts,
			})
		}
	}
	return out, nil
}

// VerifySignature checks `X-Hub-Signature-256` — `sha256=<hex(hmac_sha256(secret, body))>`.
// When FB_APP_SECRET is unset (typical local-dev), we accept everything;
// production setups should always set it.
func (FacebookProvider) VerifySignature(headers map[string]string, body []byte, cfg *config.Config, _ *models.ChannelConnection) bool {
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

func (FacebookProvider) Send(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, reply string) error {
	token := conn.Credentials["page_access_token"]
	if token == "" {
		return fmt.Errorf("fb send: missing page_access_token")
	}
	u := "https://graph.facebook.com/" + graphVersion + "/me/messages?access_token=" + url.QueryEscape(token)
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
		return fmt.Errorf("fb send: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// ── OAuth helpers ──────────────────────────────────────────────────────
//
// These are package-level functions (not provider methods) so the channels
// HTTP handler can call them directly without typing through Provider.

// FacebookLoginURL returns the URL we redirect the user to in order to
// start the Facebook Login flow. `state` is an opaque token we'll receive
// back at the callback so we can match the response to the right tenant.
//
// Scopes:
//
//   - pages_show_list   — list the user's pages
//   - pages_messaging   — send messages on behalf of those pages
//   - pages_manage_metadata — required to subscribe to webhooks
//   - business_management   — needed for some shared-page setups
func FacebookLoginURL(cfg *config.Config, state string) string {
	q := url.Values{}
	q.Set("client_id", cfg.FBAppID)
	q.Set("redirect_uri", cfg.FBOAuthRedirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	q.Set("scope", "pages_show_list,pages_messaging,pages_manage_metadata,business_management")
	return "https://www.facebook.com/" + graphVersion + "/dialog/oauth?" + q.Encode()
}

// FacebookExchangeCode swaps the OAuth `code` for a short-lived user access
// token, then immediately upgrades it to a long-lived (60-day) token.
//
// Returns the long-lived token. Errors are wrapped with enough detail to
// debug Meta's notoriously cryptic responses.
func FacebookExchangeCode(ctx context.Context, cfg *config.Config, code string) (string, error) {
	short, err := fbGraphTokenExchange(ctx, url.Values{
		"client_id":     {cfg.FBAppID},
		"client_secret": {cfg.FBAppSecret},
		"redirect_uri":  {cfg.FBOAuthRedirectURI},
		"code":          {code},
	})
	if err != nil {
		return "", fmt.Errorf("fb oauth (short-lived): %w", err)
	}
	long, err := fbGraphTokenExchange(ctx, url.Values{
		"grant_type":        {"fb_exchange_token"},
		"client_id":         {cfg.FBAppID},
		"client_secret":     {cfg.FBAppSecret},
		"fb_exchange_token": {short},
	})
	if err != nil {
		return "", fmt.Errorf("fb oauth (long-lived): %w", err)
	}
	return long, nil
}

// fbGraphTokenExchange hits /oauth/access_token with whatever params the
// caller wants — used for both code-exchange and token-extension.
func fbGraphTokenExchange(ctx context.Context, q url.Values) (string, error) {
	u := "https://graph.facebook.com/" + graphVersion + "/oauth/access_token?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s: %s", resp.Status, string(rb))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Error       *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf(out.Error.Message)
	}
	return out.AccessToken, nil
}

// FacebookListPages lists the pages the user manages, including each
// page's own access token (which is what we persist as a connection
// credential — never the user token).
func FacebookListPages(ctx context.Context, userAccessToken string) ([]models.FacebookOAuthPage, error) {
	u := "https://graph.facebook.com/" + graphVersion + "/me/accounts?fields=id,name,access_token,category&access_token=" + url.QueryEscape(userAccessToken)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fb list pages: %s: %s", resp.Status, string(rb))
	}
	var out struct {
		Data []models.FacebookOAuthPage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// FacebookSubscribePage subscribes our app to receive message webhooks for
// a specific page. Without this, FB won't deliver any events for the page
// even though the user granted permission.
//
// Idempotent — calling it again on an already-subscribed page is a no-op.
func FacebookSubscribePage(ctx context.Context, pageAccessToken, pageID string) error {
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(pageID) + "/subscribed_apps"
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
		return fmt.Errorf("fb subscribe: %s: %s", resp.Status, string(rb))
	}
	return nil
}
