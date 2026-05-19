package handlers

// Referral API — user-facing endpoints for the word-of-mouth programme.
//
//   GET  /api/v1/referral/code           → get (or auto-create) my referral code
//   GET  /api/v1/referral                → my referral stats + referred list
//   GET  /api/v1/referral/wallet         → wallet balance + last 50 transactions
//   POST /api/v1/referral/wallet/payout  → request payout or apply as bill credit

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type ReferralHandler struct {
	mongo *db.Mongo
}

func NewReferralHandler(m *db.Mongo) *ReferralHandler {
	return &ReferralHandler{mongo: m}
}

// GetCode returns the tenant owner's referral code, creating one if it
// doesn't exist yet (idempotent — safe to call on every settings page load).
//
// GET /api/v1/referral/code
func (h *ReferralHandler) GetCode(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)

	var code models.ReferralCode
	err := h.mongo.DB.Collection("referral_codes").
		FindOne(c.Context(), bson.M{"tenant_id": tid}).Decode(&code)
	if err == nil {
		return c.JSON(code)
	}
	if err != mongo.ErrNoDocuments {
		return err
	}

	// Auto-create a code for this tenant.
	// Look up the tenant name to derive a human-readable code.
	var tenant models.Tenant
	if e := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&tenant); e != nil {
		return e
	}
	codeStr, err := generateUniqueCode(c.Context(), h.mongo, tenant.Name)
	if err != nil {
		return err
	}

	code = models.ReferralCode{
		ID:        codeStr,
		TenantID:  tid,
		UserID:    uid,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := h.mongo.DB.Collection("referral_codes").InsertOne(c.Context(), code); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// Race condition — just fetch the winner.
			_ = h.mongo.DB.Collection("referral_codes").
				FindOne(c.Context(), bson.M{"tenant_id": tid}).Decode(&code)
		} else {
			return err
		}
	}
	return c.JSON(code)
}

// GetStats returns the referral overview: total referrals, total earned, and
// the list of tenants referred (newest first, capped at 100).
//
// GET /api/v1/referral
func (h *ReferralHandler) GetStats(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)

	cur, err := h.mongo.DB.Collection("referrals").Find(
		c.Context(),
		bson.M{"referrer_tenant_id": tid},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100),
	)
	if err != nil {
		return err
	}
	defer cur.Close(c.Context())

	var referrals []models.Referral
	if err := cur.All(c.Context(), &referrals); err != nil {
		return err
	}
	if referrals == nil {
		referrals = []models.Referral{}
	}

	totalEarned := 0
	for _, r := range referrals {
		totalEarned += r.TotalEarned
	}

	return c.JSON(fiber.Map{
		"total_referrals": len(referrals),
		"total_earned":    totalEarned,
		"referrals":       referrals,
	})
}

// GetWallet returns the wallet balance and the last 50 transactions.
//
// GET /api/v1/referral/wallet
func (h *ReferralHandler) GetWallet(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)

	var wallet models.Wallet
	err := h.mongo.DB.Collection("wallets").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&wallet)
	if err == mongo.ErrNoDocuments {
		// Wallet not created yet (no commissions received) — return empty.
		wallet = models.Wallet{
			ID:         tid,
			TenantID:   tid,
			Balance:    0,
			PayoutType: models.PayoutTypeManual,
		}
	} else if err != nil {
		return err
	}

	cur, err := h.mongo.DB.Collection("wallet_transactions").Find(
		c.Context(),
		bson.M{"tenant_id": tid},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(50),
	)
	if err != nil {
		return err
	}
	defer cur.Close(c.Context())

	var txns []models.WalletTransaction
	if err := cur.All(c.Context(), &txns); err != nil {
		return err
	}
	if txns == nil {
		txns = []models.WalletTransaction{}
	}

	return c.JSON(fiber.Map{
		"wallet":       wallet,
		"transactions": txns,
	})
}

type payoutReq struct {
	// PayoutType overrides the wallet's default. Omit to use wallet default.
	PayoutType string `json:"payout_type"` // "manual" | "credit"
}

// RequestPayout either records a manual payout request (for admin to action)
// or applies the balance as a credit on the next Stripe invoice.
//
// POST /api/v1/referral/wallet/payout
func (h *ReferralHandler) RequestPayout(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)

	var req payoutReq
	_ = c.BodyParser(&req)

	var wallet models.Wallet
	if err := h.mongo.DB.Collection("wallets").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&wallet); err == mongo.ErrNoDocuments {
		return fiber.NewError(fiber.StatusBadRequest, "no wallet found — no commissions earned yet")
	} else if err != nil {
		return err
	}

	if wallet.Balance <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "wallet balance is zero")
	}

	payoutType := wallet.PayoutType
	if req.PayoutType == models.PayoutTypeManual || req.PayoutType == models.PayoutTypeCredit {
		payoutType = req.PayoutType
	}

	amount := wallet.Balance
	now := time.Now().UTC()

	if payoutType == models.PayoutTypeManual {
		// Record a payout transaction and zero the balance. Admin will pay
		// out externally and confirm in the admin panel.
		txn := models.WalletTransaction{
			ID:          uuid.NewString(),
			TenantID:    tid,
			Type:        models.TxnTypePayout,
			Amount:      -amount,
			Description: fmt.Sprintf("Manual payout request — ฿%.2f", float64(amount)/100),
			CreatedAt:   now,
		}
		if _, err := h.mongo.DB.Collection("wallet_transactions").InsertOne(c.Context(), txn); err != nil {
			return err
		}
		_, err := h.mongo.DB.Collection("wallets").UpdateOne(
			c.Context(),
			bson.M{"_id": tid},
			bson.M{"$set": bson.M{"balance": 0, "updated_at": now}},
		)
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{
			"ok":      true,
			"type":    "manual",
			"amount":  amount,
			"message": "Payout request recorded. Platform admin will process it.",
		})
	}

	// Credit — apply to next Stripe invoice via a balance credit on the
	// Stripe customer. For now we just record it locally and admin applies it;
	// in a future iteration this can call stripe.CustomerBalanceTransaction.New.
	txn := models.WalletTransaction{
		ID:          uuid.NewString(),
		TenantID:    tid,
		Type:        models.TxnTypeCreditApplied,
		Amount:      -amount,
		Description: fmt.Sprintf("Bill credit applied — ฿%.2f", float64(amount)/100),
		CreatedAt:   now,
	}
	if _, err := h.mongo.DB.Collection("wallet_transactions").InsertOne(c.Context(), txn); err != nil {
		return err
	}
	_, err := h.mongo.DB.Collection("wallets").UpdateOne(
		c.Context(),
		bson.M{"_id": tid},
		bson.M{"$set": bson.M{"balance": 0, "updated_at": now}},
	)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"ok":      true,
		"type":    "credit",
		"amount":  amount,
		"message": "Credit applied. It will be deducted from your next invoice.",
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

var nonAlpha = regexp.MustCompile(`[^A-Z0-9]`)

// generateUniqueCode creates a human-readable referral code like "NAPAT24".
// It strips non-ASCII letters, uppercases, takes up to 5 chars from the
// tenant name, and appends the 2-digit year. Appends a numeric suffix on collision.
func generateUniqueCode(ctx interface{ Done() <-chan struct{} }, m *db.Mongo, tenantName string) (string, error) {
	// Extract ASCII letters only, uppercase.
	var letters []rune
	for _, r := range tenantName {
		if unicode.IsLetter(r) && r < 128 {
			letters = append(letters, unicode.ToUpper(r))
		}
	}
	base := string(letters)
	if len(base) > 5 {
		base = base[:5]
	}
	if base == "" {
		base = "REF"
	}
	year := fmt.Sprintf("%02d", time.Now().Year()%100)
	candidate := base + year

	for i := 0; i < 100; i++ {
		code := candidate
		if i > 0 {
			code = fmt.Sprintf("%s%d", candidate, i)
		}
		// Check uniqueness.
		count, err := m.DB.Collection("referral_codes").CountDocuments(
			nil, bson.M{"_id": code},
		)
		if err != nil {
			// If context is nil, try without it.
			count2, e2 := m.DB.Collection("referral_codes").CountDocuments(
				nil, bson.M{"_id": code},
			)
			if e2 != nil {
				return "", e2
			}
			count = count2
		}
		if count == 0 {
			return strings.ToUpper(nonAlpha.ReplaceAllString(code, "")), nil
		}
	}
	// Fallback: use UUID prefix.
	return strings.ToUpper(base + uuid.NewString()[:4]), nil
}

// CreditCommission credits a commission to a referrer's wallet when a referred
// tenant's invoice is paid. Called from the Stripe webhook handler.
// Safe to call concurrently — uses MongoDB $inc for atomicity.
func CreditCommission(ctx interface {
	Done() <-chan struct{}
}, m *db.Mongo, referrerTenantID, referralID, description string, amount int) error {
	now := time.Now().UTC()

	// Upsert wallet — create with this amount if it doesn't exist yet.
	_, err := m.DB.Collection("wallets").UpdateOne(
		nil,
		bson.M{"_id": referrerTenantID},
		bson.M{
			"$inc": bson.M{"balance": amount},
			"$set": bson.M{"tenant_id": referrerTenantID, "updated_at": now},
			"$setOnInsert": bson.M{
				"payout_type": models.PayoutTypeManual,
			},
		},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("credit commission wallet upsert: %w", err)
	}

	txn := models.WalletTransaction{
		ID:          uuid.NewString(),
		TenantID:    referrerTenantID,
		Type:        models.TxnTypeCommission,
		Amount:      amount,
		ReferralID:  referralID,
		Description: description,
		CreatedAt:   now,
	}
	_, err = m.DB.Collection("wallet_transactions").InsertOne(nil, txn)
	return err
}
