package models

import "time"

// ── Referral settings ──────────────────────────────────────────────────────────
//
// Single admin-configurable document stored in collection "referral_settings"
// with _id = "global". Amounts are in satang (฿1 = 100 satang) to avoid
// floating-point arithmetic.

type ReferralSettings struct {
	ID                        string    `bson:"_id" json:"id"`
	Enabled                   bool      `bson:"enabled" json:"enabled"`
	FirstCommissionAmount     int       `bson:"first_commission_amount" json:"first_commission_amount"`         // satang, default 10000 = ฿100
	RecurringCommissionAmount int       `bson:"recurring_commission_amount" json:"recurring_commission_amount"` // satang, default 5000 = ฿50
	DiscountPercent           int       `bson:"discount_percent" json:"discount_percent"`                       // default 10 = 10%
	DiscountDurationMonths    int       `bson:"discount_duration_months" json:"discount_duration_months"`       // default 12
	DefaultPayoutType         string    `bson:"default_payout_type" json:"default_payout_type"`                 // "manual" | "credit"
	UpdatedAt                 time.Time `bson:"updated_at" json:"updated_at"`
}

// DefaultReferralSettings returns sensible defaults used when no settings doc
// exists yet (first boot before an admin has touched the config).
func DefaultReferralSettings() ReferralSettings {
	return ReferralSettings{
		ID:                        "global",
		Enabled:                   true,
		FirstCommissionAmount:     10000,
		RecurringCommissionAmount: 5000,
		DiscountPercent:           10,
		DiscountDurationMonths:    12,
		DefaultPayoutType:         PayoutTypeManual,
	}
}

// ── Referral code ──────────────────────────────────────────────────────────────
//
// Auto-generated for every new tenant owner at signup.
// _id IS the code string (e.g. "NAPAT24") so lookup by code is O(1).

type ReferralCode struct {
	ID        string    `bson:"_id" json:"id"` // the code itself
	TenantID  string    `bson:"tenant_id" json:"tenant_id"`
	UserID    string    `bson:"user_id" json:"user_id"` // owner user
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}

// ── Referral ───────────────────────────────────────────────────────────────────
//
// One record per referrer→referred relationship. Created when a new tenant
// signs up using a referral code.

const (
	ReferralStatusActive = "active"
	ReferralStatusPaused = "paused" // referred tenant canceled or went past_due
)

type Referral struct {
	ID               string    `bson:"_id" json:"id"`
	Code             string    `bson:"code" json:"code"`
	ReferrerTenantID string    `bson:"referrer_tenant_id" json:"referrer_tenant_id"`
	ReferrerUserID   string    `bson:"referrer_user_id" json:"referrer_user_id"`
	ReferredTenantID string    `bson:"referred_tenant_id" json:"referred_tenant_id"`
	// ReferredTenantName is cached at creation for display in admin / user views
	// without a join. Stale if the tenant renames itself — acceptable tradeoff.
	ReferredTenantName string    `bson:"referred_tenant_name" json:"referred_tenant_name"`
	Status             string    `bson:"status" json:"status"`                     // "active" | "paused"
	CommissionCount    int       `bson:"commission_count" json:"commission_count"` // # payments where commission was credited
	TotalEarned        int       `bson:"total_earned" json:"total_earned"`          // satang lifetime
	CreatedAt          time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt          time.Time `bson:"updated_at" json:"updated_at"`
}

// ── Wallet ─────────────────────────────────────────────────────────────────────
//
// One wallet per tenant, created lazily when the first commission arrives.
// _id == tenant_id for a direct O(1) FindOne.

const (
	PayoutTypeManual = "manual" // admin pays out externally (PromptPay / bank)
	PayoutTypeCredit = "credit" // balance is deducted from the tenant's next Stripe invoice
)

type Wallet struct {
	ID         string    `bson:"_id" json:"id"` // = tenant_id
	TenantID   string    `bson:"tenant_id" json:"tenant_id"`
	Balance    int       `bson:"balance" json:"balance"`         // satang, always >= 0
	PayoutType string    `bson:"payout_type" json:"payout_type"` // "manual" | "credit"
	UpdatedAt  time.Time `bson:"updated_at" json:"updated_at"`
}

// ── Wallet transaction ─────────────────────────────────────────────────────────
//
// Immutable audit record of every balance movement.

const (
	TxnTypeCommission    = "commission"     // referral commission credited
	TxnTypePayout        = "payout"         // manual cash payout by admin
	TxnTypeCreditApplied = "credit_applied" // balance applied toward own Stripe invoice
)

type WalletTransaction struct {
	ID          string    `bson:"_id" json:"id"`
	TenantID    string    `bson:"tenant_id" json:"tenant_id"`
	Type        string    `bson:"type" json:"type"`     // commission | payout | credit_applied
	Amount      int       `bson:"amount" json:"amount"` // satang; positive = credit, negative = debit
	ReferralID  string    `bson:"referral_id,omitempty" json:"referral_id,omitempty"`
	Description string    `bson:"description" json:"description"`
	CreatedAt   time.Time `bson:"created_at" json:"created_at"`
}
