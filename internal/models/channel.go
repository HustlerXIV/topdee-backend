package models

import "time"

// Provider names — kept as constants so route handlers, the registry, and
// stored documents all spell them the same way.
const (
	ProviderFacebook  = "facebook"
	ProviderInstagram = "instagram"
	ProviderLine      = "line"
	ProviderWeb       = "web"
)

// ChannelConnection statuses.
const (
	ChannelStatusActive   = "active"
	ChannelStatusError    = "error"
	ChannelStatusDisabled = "disabled"
)

// ChannelConnection — one bound external account on a third-party platform.
//
// A tenant can have many of these per provider, subject to plan limits
// (see internal/channels/limits.go). They live in their own Mongo collection
// (`channel_connections`) rather than under `tenants.<provider>` so the data
// model doesn't have to grow a new field every time we add a platform.
//
// `external_id` is whatever the provider uses to address inbound events back
// to a single connection: Facebook page id, LINE channel id, etc. The pair
// (provider, external_id) is globally unique — the same FB page or LINE
// channel can't be claimed by two tenants at once.
//
// `credentials` holds whatever secrets the provider needs for outbound calls
// and signature verification. Stored as a flat string map so adding a new
// provider doesn't require schema changes:
//
//   facebook → { "page_access_token": "EAAB..." }
//   line     → { "channel_secret": "...", "channel_access_token": "..." }
//
// Credentials never leave the server — the JSON tag is `-`.
type ChannelConnection struct {
	ID          string            `bson:"_id" json:"id"`
	TenantID    string            `bson:"tenant_id" json:"tenant_id"`
	Provider    string            `bson:"provider" json:"provider"`
	ExternalID  string            `bson:"external_id" json:"external_id"`
	DisplayName string            `bson:"display_name" json:"display_name"`
	Credentials map[string]string `bson:"credentials" json:"-"`
	Config      map[string]any    `bson:"config,omitempty" json:"config,omitempty"`
	Status      string            `bson:"status" json:"status"`
	Error       string            `bson:"error,omitempty" json:"error,omitempty"`
	CreatedBy   string            `bson:"created_by,omitempty" json:"created_by,omitempty"`
	CreatedAt   time.Time         `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time         `bson:"updated_at" json:"updated_at"`
}

// FacebookOAuthState — one in-flight OAuth handshake. Created when the user
// hits "Connect Facebook" in the dashboard, populated by our /oauth/callback
// once Meta redirects back, and consumed by the page-picker step. Auto-expires
// after a short TTL.
//
// We store the long-lived user access token + the discovered pages here so
// the user can pick which pages to connect without re-doing OAuth. Once they
// pick (or abandon), the doc is deleted.
type FacebookOAuthState struct {
	State           string                  `bson:"_id" json:"state"`
	TenantID        string                  `bson:"tenant_id" json:"tenant_id"`
	UserID          string                  `bson:"user_id" json:"user_id"`
	UserAccessToken string                  `bson:"user_access_token" json:"-"`
	Pages           []FacebookOAuthPage     `bson:"pages" json:"pages"`
	CreatedAt       time.Time               `bson:"created_at" json:"created_at"`
	ExpiresAt       time.Time               `bson:"expires_at" json:"expires_at"`
}

// FacebookOAuthPage — one entry in the page picker. AccessToken is server-only
// (it's the per-page token we'll persist as a connection credential when the
// user picks this page).
type FacebookOAuthPage struct {
	ID          string `bson:"id" json:"id"`
	Name        string `bson:"name" json:"name"`
	Category    string `bson:"category,omitempty" json:"category,omitempty"`
	AccessToken string `bson:"access_token" json:"-"`
}

// InstagramOAuthState — mirrors FacebookOAuthState but for Instagram Business
// accounts. Uses the same Meta OAuth dance (same app, same FB_APP_ID /
// FB_APP_SECRET) with additional instagram_manage_messages scope. After the
// callback we list the Instagram Business accounts linked to the user's pages
// and let the user pick which to connect.
type InstagramOAuthState struct {
	State           string                   `bson:"_id" json:"state"`
	TenantID        string                   `bson:"tenant_id" json:"tenant_id"`
	UserID          string                   `bson:"user_id" json:"user_id"`
	UserAccessToken string                   `bson:"user_access_token" json:"-"`
	Accounts        []InstagramOAuthAccount  `bson:"accounts" json:"accounts"`
	CreatedAt       time.Time                `bson:"created_at" json:"created_at"`
	ExpiresAt       time.Time                `bson:"expires_at" json:"expires_at"`
}

// InstagramOAuthAccount — one Instagram Business account linked to a Facebook
// page. PageAccessToken is server-only; it's used to send replies via the
// Instagram Messaging API.
type InstagramOAuthAccount struct {
	// IGID is the Instagram Business Account ID (used as external_id and as
	// the send endpoint: POST /{IGID}/messages).
	IGID            string `bson:"igid" json:"igid"`
	Name            string `bson:"name" json:"name"`
	Username        string `bson:"username,omitempty" json:"username,omitempty"`
	PageID          string `bson:"page_id" json:"page_id"`
	PageAccessToken string `bson:"page_access_token" json:"-"`
}
