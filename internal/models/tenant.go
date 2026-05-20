package models

import "time"

// FacebookConnection holds per-tenant Messenger credentials. The app-level
// secret + verify token live in env (config.Config), not here.
type FacebookConnection struct {
	PageID          string    `bson:"page_id" json:"page_id"`
	PageName        string    `bson:"page_name" json:"page_name"`
	PageAccessToken string    `bson:"page_access_token" json:"-"`
	ConnectedAt     time.Time `bson:"connected_at" json:"connected_at"`
}

// LineConnection — channel secret + access token are per-tenant on LINE.
type LineConnection struct {
	ChannelID          string    `bson:"channel_id" json:"channel_id"`
	ChannelSecret      string    `bson:"channel_secret" json:"-"`
	ChannelAccessToken string    `bson:"channel_access_token" json:"-"`
	ConnectedAt        time.Time `bson:"connected_at" json:"connected_at"`
}

// BotSettings is the per-tenant override of the platform agent.
// All fields are optional — empty/zero values mean "fall back to the env default".
//
//   - Name / Language / Persona / Mode are mostly UI metadata, but Persona +
//     Language flow into the runtime prompt so the AI can pick the right tone.
//   - SystemPrompt, Model, Temperature override the env defaults when set.
//
// The orchestrator reads this on every chat turn (one tiny Mongo lookup).
type BotSettings struct {
	Name         string    `bson:"name" json:"name"`
	Language     string    `bson:"language" json:"language"`           // "th" | "en" | "mix"
	Persona      string    `bson:"persona" json:"persona"`             // "friendly" | "formal" | "fun" | "concise"
	Mode         string    `bson:"mode" json:"mode"`                   // "auto" | "suggest" | "manual"
	SystemPrompt string    `bson:"system_prompt" json:"system_prompt"` // raw editable prompt
	Model        string    `bson:"model,omitempty" json:"model,omitempty"`
	Temperature  *float64  `bson:"temperature,omitempty" json:"temperature,omitempty"`
	UpdatedAt    time.Time `bson:"updated_at" json:"updated_at"`
}

// DayHours represents one day in the weekly business-hours schedule.
// Open/Close are wall-clock times in the tenant timezone, "HH:MM" 24h.
type DayHours struct {
	Enabled bool   `bson:"enabled" json:"enabled"`
	Open    string `bson:"open" json:"open"`
	Close   string `bson:"close" json:"close"`
}

// Subscription holds billing state managed by a platform admin. Until
// real payment-provider integration ships (Stripe, Omise, etc.), this is
// the system of record — admins set status + dates by hand after taking
// payment out-of-band.
//
// `Status` lifecycle:
//
//	trialing  → active   (trial converted; admin set current_period_end)
//	trialing  → canceled (user didn't convert before trial_ends_at)
//	active    → past_due (current_period_end passed, unpaid)
//	past_due  → active   (admin set a new current_period_end)
//	active    → canceled (cancel_at_period_end + period elapsed, OR force-cancel)
type Subscription struct {
	Status            string     `bson:"status" json:"status"` // "trialing" | "active" | "past_due" | "canceled" | "paused"
	TrialEndsAt       *time.Time `bson:"trial_ends_at,omitempty" json:"trial_ends_at,omitempty"`
	CurrentPeriodEnd  *time.Time `bson:"current_period_end,omitempty" json:"current_period_end,omitempty"`
	CanceledAt        *time.Time `bson:"canceled_at,omitempty" json:"canceled_at,omitempty"`
	CancelAtPeriodEnd bool       `bson:"cancel_at_period_end" json:"cancel_at_period_end"`
	AdminNotes        string     `bson:"admin_notes" json:"admin_notes"` // free-text, e.g. "PromptPay invoice #1234"
	UpdatedAt         time.Time  `bson:"updated_at" json:"updated_at"`
}

// Subscription status constants — kept as strings to avoid typos.
const (
	SubStatusTrialing = "trialing"
	SubStatusActive   = "active"
	SubStatusPastDue  = "past_due"
	SubStatusCanceled = "canceled"
	SubStatusPaused   = "paused"
)

// BusinessHours is the workspace-level weekly schedule. Days are indexed
// by ISO weekday number: 0 = Sunday, 6 = Saturday (matches JS getDay() and
// Go time.Weekday). The orchestrator uses this to tell the AI whether the
// shop is currently open and what message to send if it isn't.
type BusinessHours struct {
	Timezone          string      `bson:"timezone" json:"timezone"`
	OutOfHoursMessage string      `bson:"out_of_hours_message" json:"out_of_hours_message"`
	Days              [7]DayHours `bson:"days" json:"days"`
	UpdatedAt         time.Time   `bson:"updated_at" json:"updated_at"`
}

type Tenant struct {
	ID           string `bson:"_id" json:"id"`
	Name         string `bson:"name" json:"name"`
	LogoURL      string `bson:"logo_url,omitempty" json:"logo_url,omitempty"`
	Timezone     string `bson:"timezone,omitempty" json:"timezone,omitempty"`
	Website      string `bson:"website,omitempty" json:"website,omitempty"`
	BusinessType string `bson:"business_type,omitempty" json:"business_type,omitempty"`
	Plan         string `bson:"plan" json:"plan"` // "free" | "starter" | "growth" | "pro" | "enterprise"
	UsageTokens  int64  `bson:"usage_tokens" json:"usage_tokens"`
	// Suspended tenants cannot use any /api/v1/* routes — flipped by a
	// platform admin to handle abuse, non-payment, or violations.
	Suspended bool `bson:"suspended" json:"suspended"`
	// Stripe linkage — set lazily on first billing action. Reused forever
	// for the customer; subscription id can change across cancel→resub.
	StripeCustomerID     string              `bson:"stripe_customer_id,omitempty" json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID string              `bson:"stripe_subscription_id,omitempty" json:"stripe_subscription_id,omitempty"`
	Subscription         *Subscription       `bson:"subscription,omitempty" json:"subscription,omitempty"`
	Bot                  *BotSettings        `bson:"bot,omitempty" json:"bot,omitempty"`
	BusinessHours        *BusinessHours      `bson:"business_hours,omitempty" json:"business_hours,omitempty"`
	Facebook             *FacebookConnection `bson:"facebook,omitempty" json:"facebook,omitempty"`
	Line                 *LineConnection     `bson:"line,omitempty" json:"line,omitempty"`
	// QuotaWarnMonth tracks when we last sent the 80% usage warning, stored as
	// "YYYY-MM" (e.g. "2026-05"). We skip sending if it matches the current
	// month so owners only get one email per billing cycle.
	QuotaWarnMonth string `bson:"quota_warn_month,omitempty" json:"-"`
	// Referral — set at signup when the tenant used a referral code.
	// ReferralCodeUsed is the code that was entered; used to look up the
	// referrer when the first invoice is paid.
	ReferralCodeUsed string `bson:"referral_code_used,omitempty" json:"referral_code_used,omitempty"`
	// ReferralDiscountExpiresAt is when the signup discount (e.g. 10% off)
	// stops being applied at Stripe Checkout. Nil = no discount.
	ReferralDiscountExpiresAt *time.Time `bson:"referral_discount_expires_at,omitempty" json:"referral_discount_expires_at,omitempty"`
	// ReferralDiscountType mirrors the programme setting at the time of signup:
	// "first_purchase" (cleared after first payment) or "duration" (valid for N months).
	// Empty is treated as "first_purchase" for backward compatibility.
	ReferralDiscountType string `bson:"referral_discount_type,omitempty" json:"referral_discount_type,omitempty"`
	CreatedAt                 time.Time  `bson:"created_at" json:"created_at"`
}

// NotificationPrefs stores which email alerts a user has opted into.
// Defaults: all true — a brand-new user gets everything until they opt out.
// Stored as a sub-document on the users collection.
type NotificationPrefs struct {
	NewChat      bool `bson:"new_chat" json:"new_chat"`
	AICantAnswer bool `bson:"ai_cant_answer" json:"ai_cant_answer"`
	QuotaWarning bool `bson:"quota_warning" json:"quota_warning"`
	DailySummary bool `bson:"daily_summary" json:"daily_summary"`
}

// DefaultNotifPrefs returns the "all enabled" state for new users.
func DefaultNotifPrefs() NotificationPrefs {
	return NotificationPrefs{
		NewChat:      true,
		AICantAnswer: true,
		QuotaWarning: true,
		DailySummary: true,
	}
}

type User struct {
	ID           string `bson:"_id" json:"id"`
	TenantID     string `bson:"tenant_id" json:"tenant_id"`
	Name         string `bson:"name" json:"name"`
	Email        string `bson:"email" json:"email"`
	PasswordHash string `bson:"password_hash" json:"-"`
	Role         string `bson:"role" json:"role"` // "owner" | "admin" | "agent" | "viewer"
	// NotifPrefs holds per-user email notification opt-in flags.
	// Missing sub-document means all enabled (default for existing users).
	NotifPrefs *NotificationPrefs `bson:"notif_prefs,omitempty" json:"notif_prefs,omitempty"`
	// IsPlatformAdmin separates Topdee staff from tenant users. A platform
	// admin can manage every workspace via /api/v1/admin/* but is otherwise
	// still tied to a tenant for normal product use.
	IsPlatformAdmin bool `bson:"is_platform_admin" json:"is_platform_admin"`
	// Suspended users cannot login. Flipped by a platform admin.
	Suspended bool `bson:"suspended" json:"suspended"`
	// Password-reset state. Token is stored as a SHA-256 hash so even if the
	// DB is read by an attacker the plaintext token (sent by email) is safe.
	PasswordResetTokenHash string     `bson:"password_reset_token_hash,omitempty" json:"-"`
	PasswordResetExpiresAt *time.Time `bson:"password_reset_expires_at,omitempty" json:"-"`
	// PrivacyAcceptedAt records when the user agreed to the Privacy Policy
	// during registration. Nil for users created before this field was added.
	PrivacyAcceptedAt *time.Time `bson:"privacy_accepted_at,omitempty" json:"privacy_accepted_at,omitempty"`
	CreatedAt         time.Time  `bson:"created_at" json:"created_at"`
}

// TeamInvite — a pending or completed invitation to join a tenant.
//
// The token is the random string the recipient uses to claim the invite via
// /accept-invite. We store its plaintext (it's already a secret on its own
// and only valid until used or expired); a stronger setup would hash it.
type TeamInvite struct {
	ID         string     `bson:"_id" json:"id"`
	TenantID   string     `bson:"tenant_id" json:"tenant_id"`
	Email      string     `bson:"email" json:"email"`
	Role       string     `bson:"role" json:"role"`
	Token      string     `bson:"token" json:"-"`       // never returned in list views
	Status     string     `bson:"status" json:"status"` // "pending" | "accepted" | "revoked" | "expired"
	InvitedBy  string     `bson:"invited_by" json:"invited_by"`
	CreatedAt  time.Time  `bson:"created_at" json:"created_at"`
	ExpiresAt  time.Time  `bson:"expires_at" json:"expires_at"`
	AcceptedAt *time.Time `bson:"accepted_at,omitempty" json:"accepted_at,omitempty"`
}

// Invite statuses kept as constants to avoid typos in queries.
const (
	InviteStatusPending  = "pending"
	InviteStatusAccepted = "accepted"
	InviteStatusRevoked  = "revoked"
	InviteStatusExpired  = "expired"
)
