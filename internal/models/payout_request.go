package models

import "time"

// ── Payout Request ────────────────────────────────────────────────────────────
//
// Created when a tenant owner submits a manual payout request. The wallet
// balance is deducted immediately on submission (optimistic hold); the admin
// then reviews and marks it as approved once the bank transfer is sent.
//
// Tax details and bank account information are captured for Thai withholding-tax
// compliance (หัก ณ ที่จ่าย). The tenant must explicitly consent to PDPA data
// retention before the request can be submitted.

const (
	PayoutRequestStatusPending  = "pending"
	PayoutRequestStatusApproved = "approved"
	PayoutRequestStatusRejected = "rejected"
)

type PayoutRequest struct {
	ID       string `bson:"_id"       json:"id"`
	TenantID string `bson:"tenant_id" json:"tenant_id"`
	Amount   int    `bson:"amount"    json:"amount"` // satang

	// ── Bank / payment details ─────────────────────────────────────────────
	BankName      string `bson:"bank_name"      json:"bank_name"`
	AccountNumber string `bson:"account_number" json:"account_number"`
	AccountName   string `bson:"account_name"   json:"account_name"`

	// ── Tax details (Thai personal/corporate tax compliance) ───────────────
	// TaxID is the 13-digit เลขประจำตัวผู้เสียภาษี (individual or corporate).
	TaxID    string `bson:"tax_id"    json:"tax_id"`
	FullName string `bson:"full_name" json:"full_name"`
	Address  string `bson:"address"   json:"address"`

	// ── PDPA consent ──────────────────────────────────────────────────────
	// ConsentGiven must be true; ConsentAt records the timestamp for audit.
	ConsentGiven bool      `bson:"consent_given" json:"consent_given"`
	ConsentAt    time.Time `bson:"consent_at"    json:"consent_at"`

	// ── Status & approval ─────────────────────────────────────────────────
	Status     string     `bson:"status"                json:"status"` // pending | approved | rejected
	ApprovedBy string     `bson:"approved_by,omitempty" json:"approved_by,omitempty"` // admin user ID
	ApprovedAt *time.Time `bson:"approved_at,omitempty" json:"approved_at,omitempty"`
	AdminNote  string     `bson:"admin_note,omitempty"  json:"admin_note,omitempty"`

	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}
