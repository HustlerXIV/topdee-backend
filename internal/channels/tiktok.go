package channels

// TikTok Business / Login Kit provider.
//
// Differences from Facebook / LINE worth knowing:
//
//   • TikTok issues short-lived (24h) access tokens that come with a
//     refresh token. We implement CredentialRefresher so the webhook
//     router can rotate them transparently before each Send.
//
//   • Connection happens via TikTok Login Kit (OAuth 2.0 with PKCE-style
//     state). The user authorizes the app, we exchange the code for an
//     access/refresh token pair, fetch the user's open_id + business
//     accounts, and let them pick which accounts to wire up.
//
//   • Signature is HMAC-SHA256 with the app-level client_secret
//     (TT_CLIENT_SECRET in env), shared across every TikTok account
//     connected to this app. Header is `TikTok-Signature` and the
//     scheme is `t=<timestamp>,s=<hex>` — we hash `timestamp + "." + body`
//     with the secret and compare in constant time.
//
//   • Outbound replies use the Business Messaging API:
//       POST https://business-api.tiktok.com/open_api/v1.3/business/message/send/
//     with a Bearer access token and the recipient's open_id.

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

// tiktokAPIVersion — bump when TikTok deprecates the current. v1.3 is GA at
// time of writing for the Business Messaging API.
const tiktokAPIVersion = "v1.3"

type TikTokProvider struct{}

func NewTikTokProvider() *TikTokProvider { return &TikTokProvider{} }

func (TikTokProvider) Name() string { return models.ProviderTikTok }

// HandshakeVerify — TikTok uses a GET handshake similar to Meta when first
// configuring a webhook URL. The console sends
// `?challenge=<token>` (sometimes `?hub.challenge=`) and expects the raw token
// echoed back. When TT_VERIFY_TOKEN is configured we additionally check
// `verify_token` for a match.
func (TikTokProvider) HandshakeVerify(q map[string]string, cfg *config.Config) (bool, string) {
	challenge := firstNonEmptyMap(q, "challenge", "hub.challenge")
	if challenge == "" {
		return false, ""
	}
	if cfg.TTVerifyToken != "" {
		token := firstNonEmptyMap(q, "verify_token", "hub.verify_token")
		if token != cfg.TTVerifyToken {
			return false, ""
		}
	}
	return true, challenge
}

func (TikTokProvider) ParseEvents(body []byte) ([]ParsedEvent, error) {
	// TikTok's webhook payload follows roughly this shape:
	//   {
	//     "client_key": "abcd",
	//     "event": "im.message.send",
	//     "create_time": 1700000000,
	//     "data": {
	//        "to_user_id": "<business open_id>",
	//        "from_user_id": "<sender open_id>",
	//        "message": { "type": "text", "content": "hi" }
	//     }
	//   }
	//
	// We accept both single-event and batched-event shapes for forward-compat.
	var single struct {
		Event      string `json:"event"`
		CreateTime int64  `json:"create_time"`
		Data       struct {
			ToUserID   string `json:"to_user_id"`
			FromUserID string `json:"from_user_id"`
			Message    *struct {
				Type    string `json:"type"`
				Content string `json:"content"`
				URL     string `json:"url"`
			} `json:"message"`
		} `json:"data"`
	}
	var batched struct {
		Events []struct {
			Event      string `json:"event"`
			CreateTime int64  `json:"create_time"`
			Data       struct {
				ToUserID   string `json:"to_user_id"`
				FromUserID string `json:"from_user_id"`
				Message    *struct {
					Type    string `json:"type"`
					Content string `json:"content"`
					URL     string `json:"url"`
				} `json:"message"`
			} `json:"data"`
		} `json:"events"`
	}

	// Try batched first; fall back to single.
	if err := json.Unmarshal(body, &batched); err == nil && len(batched.Events) > 0 {
		out := make([]ParsedEvent, 0, len(batched.Events))
		for _, e := range batched.Events {
			if !strings.HasPrefix(e.Event, "im.message") || e.Data.Message == nil {
				continue
			}
			text := strings.TrimSpace(e.Data.Message.Content)
			attachments := []models.Attachment{}
			if e.Data.Message.Type == "image" && e.Data.Message.URL != "" {
				attachments = append(attachments, models.Attachment{
					Type: "image",
					URL:  e.Data.Message.URL,
				})
			}
			if text == "" && len(attachments) == 0 {
				continue
			}
			ts := time.Unix(e.CreateTime, 0)
			if e.CreateTime == 0 {
				ts = time.Now()
			}
			out = append(out, ParsedEvent{
				ExternalChannelID: e.Data.ToUserID,
				ExternalUserID:    e.Data.FromUserID,
				Text:              text,
				Attachments:       attachments,
				Timestamp:         ts,
			})
		}
		return out, nil
	}
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(single.Event, "im.message") || single.Data.Message == nil {
		return nil, nil
	}
	text := strings.TrimSpace(single.Data.Message.Content)
	attachments := []models.Attachment{}
	if single.Data.Message.Type == "image" && single.Data.Message.URL != "" {
		attachments = append(attachments, models.Attachment{
			Type: "image",
			URL:  single.Data.Message.URL,
		})
	}
	if text == "" && len(attachments) == 0 {
		return nil, nil
	}
	ts := time.Unix(single.CreateTime, 0)
	if single.CreateTime == 0 {
		ts = time.Now()
	}
	return []ParsedEvent{{
		ExternalChannelID: single.Data.ToUserID,
		ExternalUserID:    single.Data.FromUserID,
		Text:              text,
		Attachments:       attachments,
		Timestamp:         ts,
	}}, nil
}

// VerifySignature checks `TikTok-Signature` — `t=<ts>,s=<hex(hmac_sha256(secret, ts+"."+body))>`.
// When TT_CLIENT_SECRET is unset (typical local-dev), we accept everything;
// production setups should always set it.
func (TikTokProvider) VerifySignature(headers map[string]string, body []byte, cfg *config.Config, _ *models.ChannelConnection) bool {
	if cfg.TTClientSecret == "" {
		return true
	}
	sig := headerCI(headers, "TikTok-Signature")
	if sig == "" {
		// Some TikTok deployments use X-Tiktok-Signature instead.
		sig = headerCI(headers, "X-Tiktok-Signature")
	}
	if sig == "" {
		return false
	}
	ts, hexsig := parseTikTokSignature(sig)
	if hexsig == "" {
		return false
	}
	want, err := hex.DecodeString(hexsig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(cfg.TTClientSecret))
	if ts != "" {
		mac.Write([]byte(ts))
		mac.Write([]byte("."))
	}
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// parseTikTokSignature extracts t=<ts>,s=<hex> from the header. When the
// header doesn't follow the parameterized form, it's treated as a raw hex
// digest with no timestamp prefix.
func parseTikTokSignature(sig string) (ts, hexsig string) {
	if !strings.Contains(sig, "=") {
		return "", sig
	}
	for _, part := range strings.Split(sig, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "s":
			hexsig = kv[1]
		}
	}
	return ts, hexsig
}

func (TikTokProvider) Send(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, reply string) error {
	token := conn.Credentials["access_token"]
	if token == "" {
		return fmt.Errorf("tiktok send: missing access_token (refresh failed)")
	}
	businessID := conn.Credentials["business_id"]
	if businessID == "" {
		businessID = conn.ExternalID
	}
	u := "https://business-api.tiktok.com/open_api/" + tiktokAPIVersion + "/business/message/send/"
	body, _ := json.Marshal(map[string]any{
		"business_id": businessID,
		"to_user_id":  evt.ExternalUserID,
		"message": map[string]any{
			"type":    "text",
			"content": reply,
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Access-Token", token)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tiktok send: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// SendImage implements channels.ImageSender. imageURL must be a publicly
// reachable HTTPS URL; TikTok's API fetches it server-side.
func (TikTokProvider) SendImage(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, imageURL string) error {
	token := conn.Credentials["access_token"]
	if token == "" {
		return fmt.Errorf("tiktok send image: missing access_token (refresh failed)")
	}
	businessID := conn.Credentials["business_id"]
	if businessID == "" {
		businessID = conn.ExternalID
	}
	u := "https://business-api.tiktok.com/open_api/" + tiktokAPIVersion + "/business/message/send/"
	body, _ := json.Marshal(map[string]any{
		"business_id": businessID,
		"to_user_id":  evt.ExternalUserID,
		"message": map[string]any{
			"type": "image",
			"url":  imageURL,
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Access-Token", token)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tiktok send image: %s: %s", resp.Status, string(rb))
	}
	return nil
}

// EnsureCredentials implements CredentialRefresher. TikTok access tokens are
// short-lived (~24h); we proactively refresh within 1h of expiry using the
// stored refresh_token.
func (TikTokProvider) EnsureCredentials(ctx context.Context, conn *models.ChannelConnection) (bool, error) {
	if conn == nil {
		return false, fmt.Errorf("nil conn")
	}
	if conn.Credentials == nil {
		conn.Credentials = map[string]string{}
	}
	tok := conn.Credentials["access_token"]
	expStr := conn.Credentials["access_token_expires_at"]
	if tok != "" && expStr != "" {
		if exp, err := time.Parse(time.RFC3339, expStr); err == nil {
			if time.Until(exp) > 1*time.Hour {
				return false, nil
			}
		}
	}
	refresh := conn.Credentials["refresh_token"]
	if refresh == "" {
		return false, fmt.Errorf("tiktok refresh: missing refresh_token")
	}
	clientKey := conn.Credentials["client_key"]
	clientSecret := conn.Credentials["client_secret"]
	if clientKey == "" || clientSecret == "" {
		return false, fmt.Errorf("tiktok refresh: missing client_key / client_secret on connection")
	}
	tr, err := TikTokRefreshToken(ctx, clientKey, clientSecret, refresh)
	if err != nil {
		return false, err
	}
	conn.Credentials["access_token"] = tr.AccessToken
	if tr.RefreshToken != "" {
		conn.Credentials["refresh_token"] = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		conn.Credentials["access_token_expires_at"] =
			time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	if tr.RefreshExpiresIn > 0 {
		conn.Credentials["refresh_token_expires_at"] =
			time.Now().Add(time.Duration(tr.RefreshExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return true, nil
}

// ── OAuth helpers ──────────────────────────────────────────────────────
//
// Package-level functions (not provider methods) so the channels HTTP
// handler can call them directly without typing through Provider.

// TikTokLoginURL returns the URL we redirect the user to in order to start
// the TikTok Login flow. `state` is an opaque token we'll receive back at
// the callback so we can match the response to the right tenant.
//
// Scopes:
//
//   - user.info.basic         — read open_id, display name, avatar
//   - user.info.profile       — extended profile (display_name, username)
//   - business.basic          — list managed business accounts
//   - business.messages       — receive and send Business Direct Messages
func TikTokLoginURL(cfg *config.Config, state string) string {
	q := url.Values{}
	q.Set("client_key", cfg.TTClientKey)
	q.Set("redirect_uri", cfg.TTOAuthRedirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	q.Set("scope", "user.info.basic,user.info.profile,business.basic,business.messages")
	return "https://www.tiktok.com/v2/auth/authorize/?" + q.Encode()
}

// TikTokTokenResponse is the shape returned by /v2/oauth/token/.
type TikTokTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	OpenID           string `json:"open_id"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
}

// TikTokExchangeCode swaps the OAuth `code` for an access/refresh token pair.
func TikTokExchangeCode(ctx context.Context, cfg *config.Config, code string) (*TikTokTokenResponse, error) {
	form := url.Values{
		"client_key":    {cfg.TTClientKey},
		"client_secret": {cfg.TTClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {cfg.TTOAuthRedirectURI},
	}
	return tiktokOAuthTokenCall(ctx, form)
}

// TikTokRefreshToken trades a refresh_token for a new access_token (and
// usually a fresh refresh_token too). Called by EnsureCredentials.
func TikTokRefreshToken(ctx context.Context, clientKey, clientSecret, refreshToken string) (*TikTokTokenResponse, error) {
	form := url.Values{
		"client_key":    {clientKey},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return tiktokOAuthTokenCall(ctx, form)
}

// tiktokOAuthTokenCall hits /v2/oauth/token/ with whatever params the caller
// wants — used for both code-exchange and refresh.
func tiktokOAuthTokenCall(ctx context.Context, form url.Values) (*TikTokTokenResponse, error) {
	req, err := http.NewRequestWithContext(
		ctx, "POST",
		"https://open.tiktokapis.com/v2/oauth/token/",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("tiktok oauth: %s: %s", resp.Status, string(rb))
	}
	var out TikTokTokenResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("tiktok oauth: parse: %w", err)
	}
	// TikTok sometimes wraps the payload in {"data": {...}, "error": {...}}.
	if out.AccessToken == "" {
		var wrap struct {
			Data  TikTokTokenResponse `json:"data"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rb, &wrap); err == nil && wrap.Data.AccessToken != "" {
			return &wrap.Data, nil
		}
		if wrap.Error.Code != "" && wrap.Error.Code != "ok" {
			return nil, fmt.Errorf("tiktok oauth: %s: %s", wrap.Error.Code, wrap.Error.Message)
		}
		return nil, fmt.Errorf("tiktok oauth: empty access_token")
	}
	return &out, nil
}

// TikTokListAccounts lists the business accounts the user manages. Always
// includes at least one entry for the user's own connected account so the
// flow remains useful even when no business accounts are linked yet.
func TikTokListAccounts(ctx context.Context, accessToken, openID string) ([]models.TikTokOAuthAccount, error) {
	// Step 1: fetch the user's own profile so we have a sensible default
	// account label even when the business listing returns nothing.
	profile, _ := TikTokUserInfo(ctx, accessToken)

	out := []models.TikTokOAuthAccount{}

	// Step 2: hit the business account listing endpoint. Failures are not
	// fatal — we still fall back to the profile-derived account so the user
	// can connect their personal TikTok.
	u := "https://business-api.tiktok.com/open_api/" + tiktokAPIVersion + "/business/get/?business_id=" + url.QueryEscape(openID)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err == nil {
		req.Header.Set("Access-Token", accessToken)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			var biz struct {
				Code int `json:"code"`
				Data struct {
					List []struct {
						BusinessID  string `json:"business_id"`
						DisplayName string `json:"display_name"`
						Username    string `json:"username"`
					} `json:"list"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&biz); err == nil {
				for _, b := range biz.Data.List {
					out = append(out, models.TikTokOAuthAccount{
						BusinessID:  b.BusinessID,
						DisplayName: firstNonEmptyStr(b.DisplayName, b.Username, b.BusinessID),
						Username:    b.Username,
					})
				}
			}
		}
	}

	// Always surface the user's own open_id as a fallback "account" so the
	// picker isn't empty for personal / creator accounts without a Business
	// Center setup.
	if len(out) == 0 {
		name := openID
		username := ""
		if profile != nil {
			if profile.DisplayName != "" {
				name = profile.DisplayName
			}
			username = profile.Username
		}
		out = append(out, models.TikTokOAuthAccount{
			BusinessID:  openID,
			DisplayName: name,
			Username:    username,
		})
	}
	return out, nil
}

// TikTokUserProfile is a minimal projection of /v2/user/info/.
type TikTokUserProfile struct {
	OpenID      string
	DisplayName string
	Username    string
	AvatarURL   string
}

// TikTokUserInfo fetches the user's display name + avatar with the basic
// scope. Best-effort — returns nil on any failure so callers can fall back
// to the placeholder.
func TikTokUserInfo(ctx context.Context, accessToken string) (*TikTokUserProfile, error) {
	u := "https://open.tiktokapis.com/v2/user/info/?fields=open_id,display_name,username,avatar_url"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, nil
	}
	var out struct {
		Data struct {
			User struct {
				OpenID      string `json:"open_id"`
				DisplayName string `json:"display_name"`
				Username    string `json:"username"`
				AvatarURL   string `json:"avatar_url"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Data.User.OpenID == "" {
		return nil, nil
	}
	return &TikTokUserProfile{
		OpenID:      out.Data.User.OpenID,
		DisplayName: out.Data.User.DisplayName,
		Username:    out.Data.User.Username,
		AvatarURL:   out.Data.User.AvatarURL,
	}, nil
}

// firstNonEmptyMap returns the first present, non-empty value among keys.
func firstNonEmptyMap(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// firstNonEmptyStr returns the first non-empty string in the list.
func firstNonEmptyStr(args ...string) string {
	for _, s := range args {
		if s != "" {
			return s
		}
	}
	return ""
}
