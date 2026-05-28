package channels

// Lazada Open Platform provider.
//
// Differences from the Meta-family providers worth knowing:
//
//   • Lazada is region-sharded: each seller belongs to one of TH, MY, SG,
//     ID, PH, VN. We pick the regional API host based on the OAuth
//     callback's `country` parameter and store it on the connection so
//     subsequent calls hit the right shard.
//
//   • Every API call is signed: HMAC-SHA256 of the canonical request
//     string (path + sorted-key alphabetical concat) with the app secret,
//     uppercase hex. Plus a `timestamp` (ms) and `sign_method=sha256`.
//
//   • Tokens are not Bearer'd — they go in the query string as
//     `access_token`. Token lifetime is ~7 days; refresh tokens last
//     ~30 days. We implement CredentialRefresher so the webhook router
//     rotates them transparently before each Send.
//
//   • Connection happens via Lazada's OAuth: the user authorizes our app,
//     Lazada redirects back to /webhooks/lazada/oauth/callback with a
//     code, we exchange it for tokens + a seller_id. The seller_id is the
//     external_id; there is no "picker" step because each authorization
//     binds to one seller account at a time.
//
//   • Outbound replies use Lazada Chat (IM):
//       POST /im/message/send  (signed, on the regional auth host)
//     with sender = seller, receiver = customer.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/models"
)

// lazadaAPIHosts maps country slug → regional API host. Lazada's OAuth
// callback hands the country back to us so we can pick the right one.
// Defaults to the cross-border `api.lazada.com` host when the country is
// unknown — that endpoint still works for most read APIs but not for
// chat send, hence the explicit fallback list.
var lazadaAPIHosts = map[string]string{
	"th": "https://api.lazada.co.th/rest",
	"my": "https://api.lazada.com.my/rest",
	"sg": "https://api.lazada.sg/rest",
	"id": "https://api.lazada.co.id/rest",
	"ph": "https://api.lazada.com.ph/rest",
	"vn": "https://api.lazada.vn/rest",
}

const (
	lazadaAuthHost = "https://auth.lazada.com/rest"
	// lazadaDefaultHost is used when the connection didn't capture a
	// country (legacy rows, manual seeding). The auth host accepts the
	// `/auth/*` endpoints regardless of region.
	lazadaDefaultHost = lazadaAuthHost
)

type LazadaProvider struct{}

func NewLazadaProvider() *LazadaProvider { return &LazadaProvider{} }

func (LazadaProvider) Name() string { return models.ProviderLazada }

// HandshakeVerify — Lazada doesn't do a GET handshake.
func (LazadaProvider) HandshakeVerify(_ map[string]string, _ *config.Config) (bool, string) {
	return false, ""
}

func (LazadaProvider) ParseEvents(body []byte) ([]ParsedEvent, error) {
	// Lazada chat webhook shape (simplified — Lazada publishes a few
	// variants depending on country, but the relevant fields line up):
	//
	//   {
	//     "message_type": 1,
	//     "data": {
	//        "session_id": "...",
	//        "from_account_id": "<customer>",
	//        "to_account_id":   "<seller>",
	//        "content": {"txt": "hi"} | {"img_url": "..."},
	//        "send_time": 1700000000000
	//     }
	//   }
	//
	// We accept both single-event and batched-events shapes for safety.
	var single struct {
		MessageType int `json:"message_type"`
		Data        struct {
			SessionID     string `json:"session_id"`
			FromAccountID string `json:"from_account_id"`
			ToAccountID   string `json:"to_account_id"`
			Content       struct {
				Txt    string `json:"txt"`
				Text   string `json:"text"`
				ImgURL string `json:"img_url"`
			} `json:"content"`
			SendTime int64 `json:"send_time"`
		} `json:"data"`
	}
	var batched struct {
		Events []struct {
			MessageType int `json:"message_type"`
			Data        struct {
				SessionID     string `json:"session_id"`
				FromAccountID string `json:"from_account_id"`
				ToAccountID   string `json:"to_account_id"`
				Content       struct {
					Txt    string `json:"txt"`
					Text   string `json:"text"`
					ImgURL string `json:"img_url"`
				} `json:"content"`
				SendTime int64 `json:"send_time"`
			} `json:"data"`
		} `json:"events"`
	}

	emit := func(d struct {
		SessionID     string `json:"session_id"`
		FromAccountID string `json:"from_account_id"`
		ToAccountID   string `json:"to_account_id"`
		Content       struct {
			Txt    string `json:"txt"`
			Text   string `json:"text"`
			ImgURL string `json:"img_url"`
		} `json:"content"`
		SendTime int64 `json:"send_time"`
	}) (ParsedEvent, bool) {
		text := strings.TrimSpace(d.Content.Txt)
		if text == "" {
			text = strings.TrimSpace(d.Content.Text)
		}
		attachments := []models.Attachment{}
		if d.Content.ImgURL != "" {
			attachments = append(attachments, models.Attachment{
				Type: "image",
				URL:  d.Content.ImgURL,
			})
		}
		if text == "" && len(attachments) == 0 {
			return ParsedEvent{}, false
		}
		ts := time.Now()
		if d.SendTime > 0 {
			// Lazada sends milliseconds; tolerate seconds too.
			if d.SendTime > 1e12 {
				ts = time.UnixMilli(d.SendTime)
			} else {
				ts = time.Unix(d.SendTime, 0)
			}
		}
		return ParsedEvent{
			ExternalChannelID: d.ToAccountID, // seller_id = our connection
			ExternalUserID:    d.FromAccountID,
			Text:              text,
			Attachments:       attachments,
			Timestamp:         ts,
		}, true
	}

	if err := json.Unmarshal(body, &batched); err == nil && len(batched.Events) > 0 {
		out := make([]ParsedEvent, 0, len(batched.Events))
		for _, e := range batched.Events {
			// Type 1 = chat text/image; outbound seller-originated
			// messages also arrive here, drop those.
			if e.MessageType != 0 && e.MessageType != 1 {
				continue
			}
			if ev, ok := emit(e.Data); ok {
				out = append(out, ev)
			}
		}
		return out, nil
	}
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, err
	}
	if single.MessageType != 0 && single.MessageType != 1 {
		return nil, nil
	}
	if ev, ok := emit(single.Data); ok {
		return []ParsedEvent{ev}, nil
	}
	return nil, nil
}

// VerifySignature checks the `X-Lazop-Signature` header (or the lowercased
// `lazop-signature` variant) — Lazada signs notification payloads with
// HMAC-SHA256 over the raw body using the app secret, uppercase hex.
func (LazadaProvider) VerifySignature(headers map[string]string, body []byte, cfg *config.Config, _ *models.ChannelConnection) bool {
	if cfg.LZAppSecret == "" {
		return true
	}
	sig := headerCI(headers, "X-Lazop-Signature")
	if sig == "" {
		sig = headerCI(headers, "Lazop-Signature")
	}
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(cfg.LZAppSecret))
	mac.Write(body)
	want := strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
	return hmac.Equal([]byte(want), []byte(strings.ToUpper(sig)))
}

func (LazadaProvider) Send(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, reply string) error {
	token := conn.Credentials["access_token"]
	if token == "" {
		return fmt.Errorf("lazada send: missing access_token (refresh failed)")
	}
	host := lazadaHostForConn(conn)
	params := map[string]string{
		"app_key":      conn.Credentials["app_key"],
		"sign_method":  "sha256",
		"timestamp":    strconv.FormatInt(time.Now().UnixMilli(), 10),
		"access_token": token,
		"to_account_id": evt.ExternalUserID,
		// Lazada uses `template_id` for typed messages; we send a plain
		// text by leaving template empty and providing `content`.
		"content": reply,
	}
	return lazadaSignedPost(ctx, host, "/im/message/send", conn.Credentials["app_secret"], params)
}

// SendImage implements channels.ImageSender for Lazada Chat.
func (LazadaProvider) SendImage(ctx context.Context, conn *models.ChannelConnection, evt ParsedEvent, imageURL string) error {
	token := conn.Credentials["access_token"]
	if token == "" {
		return fmt.Errorf("lazada send image: missing access_token (refresh failed)")
	}
	host := lazadaHostForConn(conn)
	params := map[string]string{
		"app_key":       conn.Credentials["app_key"],
		"sign_method":   "sha256",
		"timestamp":     strconv.FormatInt(time.Now().UnixMilli(), 10),
		"access_token":  token,
		"to_account_id": evt.ExternalUserID,
		"img_url":       imageURL,
		"message_type":  "image",
	}
	return lazadaSignedPost(ctx, host, "/im/message/send", conn.Credentials["app_secret"], params)
}

// EnsureCredentials implements CredentialRefresher. Lazada access tokens
// last ~7 days; we refresh proactively when within 24h of expiry.
func (LazadaProvider) EnsureCredentials(ctx context.Context, conn *models.ChannelConnection) (bool, error) {
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
			if time.Until(exp) > 24*time.Hour {
				return false, nil
			}
		}
	}
	refresh := conn.Credentials["refresh_token"]
	if refresh == "" {
		return false, fmt.Errorf("lazada refresh: missing refresh_token")
	}
	appKey := conn.Credentials["app_key"]
	appSecret := conn.Credentials["app_secret"]
	if appKey == "" || appSecret == "" {
		return false, fmt.Errorf("lazada refresh: missing app_key / app_secret on connection")
	}
	tr, err := LazadaRefreshToken(ctx, appKey, appSecret, refresh)
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

// lazadaHostForConn picks the regional API host based on the connection's
// country tag, falling back to the auth host when country is missing
// (still usable for /auth/* but not for /im/*).
func lazadaHostForConn(conn *models.ChannelConnection) string {
	if conn == nil {
		return lazadaDefaultHost
	}
	country := strings.ToLower(conn.Credentials["country"])
	if h, ok := lazadaAPIHosts[country]; ok {
		return h
	}
	return lazadaDefaultHost
}

// ── OAuth + signed requests ────────────────────────────────────────────

// LazadaLoginURL returns the URL we redirect the user to for the OAuth
// dance. `state` is opaque to Lazada and echoed back at the callback so
// we can match the response to the right tenant.
func LazadaLoginURL(cfg *config.Config, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("force_auth", "true")
	q.Set("client_id", cfg.LZAppKey)
	q.Set("redirect_uri", cfg.LZOAuthRedirectURI)
	q.Set("state", state)
	return "https://auth.lazada.com/oauth/authorize?" + q.Encode()
}

// LazadaTokenResponse is the shape returned by /auth/token/create and
// /auth/token/refresh.
type LazadaTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	Account          string `json:"account"`
	Country          string `json:"country"`
	CountryUserInfo  []struct {
		Country    string `json:"country"`
		UserID     string `json:"user_id"`
		SellerID   string `json:"seller_id"`
		ShortCode  string `json:"short_code"`
	} `json:"country_user_info"`
}

// LazadaExchangeCode swaps the OAuth `code` for an access/refresh token
// pair. The response includes the country list so the caller can persist
// the regional host alongside the credentials.
func LazadaExchangeCode(ctx context.Context, cfg *config.Config, code string) (*LazadaTokenResponse, error) {
	params := map[string]string{
		"app_key":     cfg.LZAppKey,
		"code":        code,
		"sign_method": "sha256",
		"timestamp":   strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	return lazadaSignedPostJSON[LazadaTokenResponse](
		ctx, lazadaAuthHost, "/auth/token/create", cfg.LZAppSecret, params,
	)
}

// LazadaRefreshToken trades a refresh_token for a new access/refresh pair.
func LazadaRefreshToken(ctx context.Context, appKey, appSecret, refreshToken string) (*LazadaTokenResponse, error) {
	params := map[string]string{
		"app_key":       appKey,
		"refresh_token": refreshToken,
		"sign_method":   "sha256",
		"timestamp":     strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	return lazadaSignedPostJSON[LazadaTokenResponse](
		ctx, lazadaAuthHost, "/auth/token/refresh", appSecret, params,
	)
}

// LazadaSellerInfo fetches the seller's display name + shop name so the
// dashboard shows "Connected: <shop>" instead of just a numeric seller id.
// Best-effort — callers may ignore the error.
func LazadaSellerInfo(ctx context.Context, conn *models.ChannelConnection) (name, shop string, err error) {
	host := lazadaHostForConn(conn)
	params := map[string]string{
		"app_key":      conn.Credentials["app_key"],
		"access_token": conn.Credentials["access_token"],
		"sign_method":  "sha256",
		"timestamp":    strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	type sellerResp struct {
		Data struct {
			Name     string `json:"name"`
			ShopName string `json:"short_code"`
		} `json:"data"`
	}
	resp, err := lazadaSignedGetJSON[sellerResp](ctx, host, "/seller/get", conn.Credentials["app_secret"], params)
	if err != nil || resp == nil {
		return "", "", err
	}
	return resp.Data.Name, resp.Data.ShopName, nil
}

// lazadaSignedPost issues a signed POST with form-encoded params. Used
// for outbound message sends where we don't need to parse the response.
func lazadaSignedPost(ctx context.Context, host, path, appSecret string, params map[string]string) error {
	params["sign"] = lazadaSign(path, appSecret, params)
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", host+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("lazada %s: %s: %s", path, resp.Status, string(rb))
	}
	// Lazada always returns 200, errors live in the body:
	//   {"code":"...","type":"ISP","message":"..."}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rb, &envelope); err == nil && envelope.Code != "" && envelope.Code != "0" && !strings.EqualFold(envelope.Code, "success") {
		return fmt.Errorf("lazada %s: %s: %s", path, envelope.Code, envelope.Message)
	}
	return nil
}

// lazadaSignedPostJSON is the same as lazadaSignedPost but unmarshals the
// response into the caller-supplied type. Generic to avoid hand-typed
// wrappers for every endpoint.
func lazadaSignedPostJSON[T any](ctx context.Context, host, path, appSecret string, params map[string]string) (*T, error) {
	params["sign"] = lazadaSign(path, appSecret, params)
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", host+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("lazada %s: %s: %s", path, resp.Status, string(rb))
	}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rb, &envelope); err == nil && envelope.Code != "" && envelope.Code != "0" && !strings.EqualFold(envelope.Code, "success") {
		return nil, fmt.Errorf("lazada %s: %s: %s", path, envelope.Code, envelope.Message)
	}
	var out T
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("lazada %s: parse: %w", path, err)
	}
	return &out, nil
}

// lazadaSignedGetJSON issues a signed GET with the params as the query
// string. Used for read endpoints.
func lazadaSignedGetJSON[T any](ctx context.Context, host, path, appSecret string, params map[string]string) (*T, error) {
	params["sign"] = lazadaSign(path, appSecret, params)
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	u := host + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("lazada %s: %s: %s", path, resp.Status, string(rb))
	}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rb, &envelope); err == nil && envelope.Code != "" && envelope.Code != "0" && !strings.EqualFold(envelope.Code, "success") {
		return nil, fmt.Errorf("lazada %s: %s: %s", path, envelope.Code, envelope.Message)
	}
	var out T
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("lazada %s: parse: %w", path, err)
	}
	return &out, nil
}

// lazadaSign computes the HMAC-SHA256 signature for a Lazada API call.
// Canonical string = api path + concat(sorted-key alphabetical of
// key+value pairs, no separators). Uppercase hex.
func lazadaSign(apiPath, appSecret string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "sign" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(apiPath)
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(params[k])
	}

	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(sb.String()))
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
}

