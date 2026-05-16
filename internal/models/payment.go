package models

import "time"

// Payment records a completed one-time payment (currently PromptPay only).
// Card/subscription renewals are tracked via Stripe Invoices and are not
// stored here — we pull those directly from the Stripe API.
//
// The document _id is the Stripe Checkout session ID, which makes all writes
// idempotent: both the SyncCheckoutSession pull path and the webhook backup
// can safely upsert the same session without creating duplicate records.
type Payment struct {
	// ID is the Stripe Checkout session ID (cs_live_xxx / cs_test_xxx).
	ID          string    `bson:"_id"         json:"id"`
	TenantID    string    `bson:"tenant_id"   json:"tenant_id"`
	Source      string    `bson:"source"      json:"source"`       // "promptpay"
	Plan        string    `bson:"plan"        json:"plan"`         // plan slug e.g. "starter"
	DisplayName string    `bson:"display_name" json:"display_name"` // e.g. "Starter"
	Interval    string    `bson:"interval"    json:"interval"`     // "month" | "year"
	Amount      int64     `bson:"amount"      json:"amount"`       // satang / smallest unit
	Currency    string    `bson:"currency"    json:"currency"`     // "thb"
	Status      string    `bson:"status"      json:"status"`       // "paid"
	Description string    `bson:"description" json:"description"`  // "Starter — 1 Month"
	PeriodStart time.Time `bson:"period_start" json:"period_start"`
	PeriodEnd   time.Time `bson:"period_end"  json:"period_end"`
	ReceiptURL  string    `bson:"receipt_url,omitempty" json:"receipt_url,omitempty"`
	CreatedAt   time.Time `bson:"created_at"  json:"created_at"`
}
