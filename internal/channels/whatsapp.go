package channels

// WhatsApp Business Cloud API provider.
//
// Key differences from Facebook / Instagram (same Meta platform):
//
//   • Webhook payload has "object": "whatsapp_business_account" and the
//     interesting events sit at entry[].changes[].value.messages[] rather
//     than entry[].messaging[]. We translate that into ParsedEvent so the
//     orchestrator never has to care.
//
//   • Signature verification reuses FB_APP_SECRET via HMAC-SHA256 with
//     X-Hub-Signature-256 — same as the Facebook / Instagram providers.
//
//   • Handshake verification reuses the same FB_VERIFY_TOKEN.
//
//   • Connection happens via Facebook Login with the WhatsApp Embedded
//     Signup scopes:
//       whatsapp_business_management, whatsapp_business_messaging,
//       business_management
//     plus the standard pages_show_list to discover the user's WhatsApp
//     Business Accounts (WABAs) and their phone numbers.
//
//   • Outbound replies use the Cloud API:
//       POST https://graph.facebook.com/v20.0/{phone-number-id}/messages
//     with `Authorization: Bearer <access_token>` and a `messaging_product:
//     "whatsapp"` body. The phone-number-id is the external_id; the
//     customer's wa-id (a phone number) is the recipient.

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
	"strconv"
	"strings"
	"time"

	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/models"
)

type WhatsAppProvider struct{}

func NewWhatsAppProvider() *WhatsAppProvider { return &WhatsAppProvider{} }

func (WhatsAppProvider) Name() string { return models.ProviderWhatsApp }

// HandshakeVerify — same hub.challenge dance as Facebook / Instagram.
func (WhatsAppProvider) HandshakeVerify(q map[string]string, cfg *config.Config) (bool, string) {
	if q["hub.mode"] == "subscribe" && cfg.FBVerifyToken != "" && q["hub.verify_token"] == cfg.FBVerifyToken {
		return true, q["hub.challenge"]
	}
	return false, ""
}

func (WhatsAppProvider) ParseEvents(body []byte) ([]ParsedEvent, error) {
	// Cloud API shape:
	//   { "object": "whatsapp_business_account",
	//     "entry": [{
	//       "id": "<WABA_ID>",
	//       "changes": [{
	//         "field": "messages",
	//         "value": {
	//           "messaging_product": "whatsapp",
	//           "metadata": { "display_phone_number": "...", "phone_number_id": "..." },
	//           "messages": [{
	//             "from": "<CUSTOMER_PHONE>",
	//             "id": "wamid.xxx",
	//             "timestamp": "1700000000",
	//             "type": "text",
	//             "text": { "body": "hi" }
	//           }]
	//         }
	//       }]
	//     }]
	//   }
	var p struct {
		Object string `json:"object"`
		Entry  []struct {
			ID      string `json:"id"`
			Changes []struct {
				Field string `json:"field"`
				Value struct {
					Metadata struct {
						DisplayPhoneNumber string `json:"display_phone_number"`
						PhoneNumberID      string `json:"phone_number_id"`
					} `json:"metadata"`
					Messages []struct {
						From      string `json:"from"`
						ID        string `json:"id"`
						Timestamp string `json:"timestamp"`
						Type      string `json:"type"`
						Text      *struct {
							Body string `json:"body"`
						} `json:"text"`
						Image *struct {
							ID       string `json:"id"`
							MimeType string `json:"mime_type"`
							SHA256   string `json:"sha256"`
						} `json:"image"`
					} `json:"messages"`
					// `statuses` (delivery receipts) and `errors` are
					// also delivered to this endpoint — we ignore them
					// at the parse step; only `messages` events flow on.
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	if p.Object != "whatsapp_business_account" {
		return nil, nil
	}

	out := []ParsedEvent{}
	for _, e := range p.Entry {
		for _, ch := range e.Changes {
			if ch.Field != "messages" {
				continue
			}
			for _, m := range ch.Value.Messages {
				text := ""
				if m.Text != nil {
					text = strings.TrimSpace(m.Text.Body)
				}
				attachments := []models.Attachment{}
				if m.Type == "image" && m.Image != nil && m.Image.ID != "" {
					ct := m.Image.MimeType
					if ct == "" {
						ct = "image/jpeg"
					}
					// The customer's image media has to be fetched from
					// the Cloud API by id; we surface a proxy URL the
					// inbox can use without leaking the access token.
					attachments = append(attachments, models.Attachment{
						ID:          m.Image.ID,
						Type:        "image",
						URL:         "/api/v1/inbox/media/" + url.PathEscape(m.Image.ID),
						ContentType: ct,
					})
				}
				if text == "" && len(attachments) == 0 {
					continue
				}
				ts := time.Now()
				if m.Timestamp != "" {
					if n, err := strconv.ParseInt(m.Timestamp, 10, 64); err == nil {
						ts = time.Unix(n, 0)
					}
				}
				out = append(out, ParsedEvent{
					// The connection is keyed on the phone-number-id, not
					// the WABA id — multiple numbers can belong to one
					// WABA, and the customer expects a reply from the
					// number they messaged.
					ExternalChannelID: ch.Value.Metadata.PhoneNumberID,
					ExternalUserID:    m.From,
					Text:              text,
					Attachments:       attachments,
					Timestamp:         ts,
				})
			}
		}
	}
	return out, nil
}

// VerifySignature — identical to Facebook / Instagram: HMAC-SHA256 over the
// raw body with FB_APP_SECRET, checked against X-Hub-Signature-256.
func (WhatsAppProvider) VerifySignature(headers map[string]string, body []byte, cfg *config.Config, _ *models.ChannelConnection) bool {
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

// Send dispatches a text reply to the customer via the Cloud API.
// conn.Credentials must contain:
//
//	"access_token"        — the WABA / system user access token
//	"phone_number_id"     — the sender phone-number-id (external_id mirror)
func (WhatsAppProvider) Send(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, reply string) error {
	token := conn.Credentials["access_token"]
	phoneNumberID := conn.Credentials["phone_number_id"]
	if phoneNumberID == "" {
		phoneNumberID = conn.ExternalID
	}
	if token == "" || phoneNumberID == "" {
		return fmt.Errorf("whatsapp send: missing access_token or phone_number_id")
	}
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(phoneNumberID) + "/messages"
	body, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                evt.ExternalUserID,
		"type":              "text",
		"text": map[string]any{
			"preview_url": false,
			"body":        reply,
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
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
		return fmt.Errorf("whatsapp send: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// SendImage implements channels.ImageSender using the Cloud API's image
// message type. imageURL must be a public HTTPS URL — the Cloud API will
// fetch and re-host it.
func (WhatsAppProvider) SendImage(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, imageURL string) error {
	token := conn.Credentials["access_token"]
	phoneNumberID := conn.Credentials["phone_number_id"]
	if phoneNumberID == "" {
		phoneNumberID = conn.ExternalID
	}
	if token == "" || phoneNumberID == "" {
		return fmt.Errorf("whatsapp send image: missing access_token or phone_number_id")
	}
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(phoneNumberID) + "/messages"
	body, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                evt.ExternalUserID,
		"type":              "image",
		"image": map[string]any{
			"link": imageURL,
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
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
		return fmt.Errorf("whatsapp send image: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// ── OAuth helpers ──────────────────────────────────────────────────────
//
// WhatsApp Cloud connection uses the Embedded Signup variant of Facebook
// Login, which adds WhatsApp-specific scopes on top of the standard Meta
// app. The user authorizes, we exchange the code, list their WhatsApp
// Business Accounts (WABAs), then list each WABA's phone numbers and let
// them pick which numbers to connect.

// WhatsAppLoginURL returns the Meta OAuth URL with the scopes needed for
// WhatsApp Business Messaging.
func WhatsAppLoginURL(cfg *config.Config, state string) string {
	q := url.Values{}
	q.Set("client_id", cfg.FBAppID)
	q.Set("redirect_uri", cfg.WAOAuthRedirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	q.Set("scope", "whatsapp_business_management,whatsapp_business_messaging,business_management,pages_show_list")
	return "https://www.facebook.com/" + graphVersion + "/dialog/oauth?" + q.Encode()
}

// WhatsAppExchangeCode swaps an OAuth code for a long-lived user token —
// reuses the same token-exchange logic as Facebook (same app).
func WhatsAppExchangeCode(ctx context.Context, cfg *config.Config, code string) (string, error) {
	short, err := fbGraphTokenExchange(ctx, url.Values{
		"client_id":     {cfg.FBAppID},
		"client_secret": {cfg.FBAppSecret},
		"redirect_uri":  {cfg.WAOAuthRedirectURI},
		"code":          {code},
	})
	if err != nil {
		return "", fmt.Errorf("wa oauth (short-lived): %w", err)
	}
	long, err := fbGraphTokenExchange(ctx, url.Values{
		"grant_type":        {"fb_exchange_token"},
		"client_id":         {cfg.FBAppID},
		"client_secret":     {cfg.FBAppSecret},
		"fb_exchange_token": {short},
	})
	if err != nil {
		return "", fmt.Errorf("wa oauth (long-lived): %w", err)
	}
	return long, nil
}

// WhatsAppListPhoneNumbers walks every WhatsApp Business Account the user
// owns and emits one entry per attached phone number. Each number is what
// the tenant will eventually pick from the connect picker.
//
// Failures on a single WABA are logged-and-skipped — partial discovery is
// always more useful than an outright error.
func WhatsAppListPhoneNumbers(ctx context.Context, userAccessToken string) ([]models.WhatsAppOAuthPhoneNumber, error) {
	// Step 1: discover the user's WABAs via the businesses endpoint. Meta
	// exposes them under /me/businesses → owned_whatsapp_business_accounts.
	bizURL := "https://graph.facebook.com/" + graphVersion +
		"/me/businesses?fields=id,name,owned_whatsapp_business_accounts{id,name}&access_token=" +
		url.QueryEscape(userAccessToken)
	req, err := http.NewRequestWithContext(ctx, "GET", bizURL, nil)
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
		return nil, fmt.Errorf("wa list businesses: %s: %s", resp.Status, string(rb))
	}
	var bizResp struct {
		Data []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Wabas struct {
				Data []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"data"`
			} `json:"owned_whatsapp_business_accounts"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bizResp); err != nil {
		return nil, fmt.Errorf("wa list businesses decode: %w", err)
	}

	out := []models.WhatsAppOAuthPhoneNumber{}
	for _, biz := range bizResp.Data {
		for _, waba := range biz.Wabas.Data {
			nums, err := whatsappListWABAPhoneNumbers(ctx, userAccessToken, waba.ID)
			if err != nil {
				// Best effort: skip this WABA but keep going.
				continue
			}
			for i := range nums {
				nums[i].WABAID = waba.ID
				nums[i].WABAName = waba.Name
				nums[i].BusinessID = biz.ID
				out = append(out, nums[i])
			}
		}
	}
	return out, nil
}

// whatsappListWABAPhoneNumbers fetches the phone numbers under one WABA.
// Returns minimal info — caller fills in WABAID / WABAName / BusinessID.
func whatsappListWABAPhoneNumbers(ctx context.Context, accessToken, wabaID string) ([]models.WhatsAppOAuthPhoneNumber, error) {
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(wabaID) +
		"/phone_numbers?fields=id,display_phone_number,verified_name,quality_rating&access_token=" +
		url.QueryEscape(accessToken)
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
		return nil, fmt.Errorf("wa list phone numbers: %s: %s", resp.Status, string(rb))
	}
	var pn struct {
		Data []struct {
			ID                 string `json:"id"`
			DisplayPhoneNumber string `json:"display_phone_number"`
			VerifiedName       string `json:"verified_name"`
			QualityRating      string `json:"quality_rating"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pn); err != nil {
		return nil, fmt.Errorf("wa list phone numbers decode: %w", err)
	}
	out := make([]models.WhatsAppOAuthPhoneNumber, 0, len(pn.Data))
	for _, n := range pn.Data {
		out = append(out, models.WhatsAppOAuthPhoneNumber{
			PhoneNumberID:      n.ID,
			DisplayPhoneNumber: n.DisplayPhoneNumber,
			VerifiedName:       n.VerifiedName,
			QualityRating:      n.QualityRating,
		})
	}
	return out, nil
}

// WhatsAppSubscribeWABA subscribes our Meta app to receive `messages`
// events for the given WABA. Without this Meta won't deliver any
// webhooks even after the user authorized the app.
//
//	POST /{waba-id}/subscribed_apps
//
// Idempotent — safe to call on an already-subscribed WABA.
func WhatsAppSubscribeWABA(ctx context.Context, accessToken, wabaID string) error {
	if accessToken == "" {
		return fmt.Errorf("wa subscribe: access token is empty")
	}
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(wabaID) + "/subscribed_apps"
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader("access_token="+url.QueryEscape(accessToken)))
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
		return fmt.Errorf("wa subscribe: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// WhatsAppRegisterPhoneNumber finishes the phone-number activation step
// (`/messages` won't accept traffic until the number is registered with
// the Cloud API). Optional — most numbers brought in via Embedded Signup
// are pre-registered, but calling this idempotently lets us recover from
// half-onboarded numbers without a manual step.
//
//	POST /{phone-number-id}/register
//	{ "messaging_product": "whatsapp", "pin": "<6-digit>" }
//
// We pass a deterministic PIN derived from the phone number for the
// no-frills case; production setups should let the seller pick their
// own PIN.
func WhatsAppRegisterPhoneNumber(ctx context.Context, accessToken, phoneNumberID, pin string) error {
	if pin == "" {
		pin = "000000"
	}
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(phoneNumberID) + "/register"
	body, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"pin":               pin,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Already-registered numbers return a specific error code; we
		// surface it so the caller can decide whether to treat as fatal.
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("wa register: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// WhatsAppMediaURL resolves a Cloud API media id to a temporary download
// URL. The URL is short-lived and must be fetched server-side with the
// access token; do not return it to the browser. Used by the inbox to
// stream image / video previews to the dashboard.
func WhatsAppMediaURL(ctx context.Context, accessToken, mediaID string) (string, error) {
	u := "https://graph.facebook.com/" + graphVersion + "/" + url.PathEscape(mediaID) + "?access_token=" + url.QueryEscape(accessToken)
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
		return "", fmt.Errorf("wa media: %s: %s", resp.Status, string(rb))
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.URL, nil
}
