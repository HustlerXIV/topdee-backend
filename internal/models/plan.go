package models

import "time"

// Plan defines a subscription tier on the Topdee platform. Admins manage
// plans via /api/v1/admin/plans; tenants are assigned a plan by name.
//
// Channel limits are stored as a generic map keyed by provider slug
// ("facebook", "line", "instagram", "shopee", …) so new providers never
// require a schema migration — the admin just adds the key.
//
// A value of -1 means unlimited for any numeric limit.
type Plan struct {
	// ID is the machine slug used in tenant.plan ("free", "starter", …).
	// Chosen by the admin; immutable after tenants are assigned.
	ID          string     `bson:"_id"         json:"id"`
	DisplayName string     `bson:"display_name" json:"display_name"`
	Description string     `bson:"description"  json:"description"`
	Price       float64    `bson:"price"        json:"price"`    // display price
	Currency    string     `bson:"currency"     json:"currency"` // e.g. "THB"
	IsActive    bool       `bson:"is_active"    json:"is_active"`
	// IsPublic controls whether the plan appears on the public pricing page
	// and tenant billing page. Set false to create a hidden/custom plan
	// that can only be assigned manually by an admin.
	IsPublic       bool       `bson:"is_public"       json:"is_public"`
	// IsRecommended highlights this plan with a "Popular" badge on the
	// pricing page. At most one plan should be recommended at a time.
	IsRecommended  bool       `bson:"is_recommended"  json:"is_recommended"`
	SortOrder   int        `bson:"sort_order"   json:"sort_order"`
	// StripePriceID is the monthly recurring Stripe Price id (price_xxx).
	// StripePriceIDYearly is the annual recurring Stripe Price id.
	// Both are set via Admin → Plans; leave empty for free/custom plans.
	StripePriceID       string `bson:"stripe_price_id,omitempty"        json:"stripe_price_id,omitempty"`
	StripePriceIDYearly string `bson:"stripe_price_id_yearly,omitempty" json:"stripe_price_id_yearly,omitempty"`
	// YearlyPrice is the display price charged per year (e.g. 9900 for ฿9,900/yr).
	// Set this to whatever the Stripe yearly price actually charges.
	YearlyPrice float64 `bson:"yearly_price,omitempty" json:"yearly_price,omitempty"`
	// YearlySavingLabel is a short badge shown next to the yearly option,
	// e.g. "2 months free", "Save 17%", "Best value". Leave empty to hide.
	YearlySavingLabel string `bson:"yearly_saving_label,omitempty" json:"yearly_saving_label,omitempty"`
	// ExpiryDays is the number of days after which a tenant on this plan
	// loses access. 0 means no expiry (access forever). When set, the
	// register handler automatically stamps subscription.trial_ends_at.
	ExpiryDays  int        `bson:"expiry_days"  json:"expiry_days"`
	Limits      PlanLimits `bson:"limits"       json:"limits"`
	CreatedAt   time.Time  `bson:"created_at"   json:"created_at"`
	UpdatedAt   time.Time  `bson:"updated_at"   json:"updated_at"`
}

// ChannelLimitMode toggles between two ways of capping a tenant's channel
// connections on a plan.
//
//   - ChannelLimitModePerProvider — each provider has its own cap, set in
//     PlanLimits.Channels (the original behavior; default when unset).
//     Example: facebook=3, line=1, instagram=0 → up to 3 FB, 1 LINE, no IG.
//
//   - ChannelLimitModeTotal — a single total cap (PlanLimits.TotalChannels)
//     bounds the *sum* of connections across all providers. The customer
//     picks which providers to use, up to the total. Per-provider entries
//     with a value of 0 still hide that provider (lets admin gate features
//     like "no Instagram on the free tier" even in total-cap mode).
const (
	ChannelLimitModePerProvider = "per_provider"
	ChannelLimitModeTotal       = "total"
)

// PlanLimits bundles all the caps for a plan tier.
type PlanLimits struct {
	// ChannelLimitMode selects between per-provider caps and a single total
	// cap. Empty string is treated as "per_provider" so existing plan
	// documents written before this field existed keep working unchanged.
	ChannelLimitMode string `bson:"channel_limit_mode,omitempty" json:"channel_limit_mode,omitempty"`

	// TotalChannels is the cap on the SUM of channel connections across
	// every provider — used only when ChannelLimitMode == "total".
	// -1 = unlimited. Ignored in per-provider mode.
	TotalChannels int `bson:"total_channels,omitempty" json:"total_channels,omitempty"`

	// Channels is keyed by provider slug. Value is the max number of
	// connections of that provider a tenant may have. -1 = unlimited.
	// Example: {"facebook": 3, "line": 1, "instagram": 0}
	//
	// In "total" mode this map is still consulted but only as a *visibility*
	// gate: a value of 0 hides the provider from the picker. Non-zero
	// values are ignored for capping in total mode.
	Channels map[string]int `bson:"channels" json:"channels"`

	// Members is the maximum number of users (team members) per workspace.
	// -1 = unlimited.
	Members int `bson:"members" json:"members"`

	// MessagesPerMonth is the cap on AI-handled inbound messages per
	// calendar month. -1 = unlimited.
	MessagesPerMonth int `bson:"messages_per_month" json:"messages_per_month"`

	// KnowledgeBases is the maximum number of knowledge bases a tenant
	// may create. -1 = unlimited.
	KnowledgeBases int `bson:"knowledge_bases" json:"knowledge_bases"`

	// StorageMB is the total file-upload storage cap in megabytes.
	// -1 = unlimited.
	StorageMB int `bson:"storage_mb" json:"storage_mb"`
}
