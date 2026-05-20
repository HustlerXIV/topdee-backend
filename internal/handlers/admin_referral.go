package handlers

// Admin referral API — platform-admin endpoints for the referral programme.
//
//   GET  /api/v1/admin/referral/settings            → current settings
//   PUT  /api/v1/admin/referral/settings            → update settings
//   GET  /api/v1/admin/referral/referrals           → all referrals (newest first)
//   GET  /api/v1/admin/referral/wallets             → all wallets with balance > 0
//   POST /api/v1/admin/referral/wallets/:id/payout  → mark manual payout as done

import (
	"context"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type AdminReferralHandler struct {
	mongo *db.Mongo
}

func NewAdminReferralHandler(m *db.Mongo) *AdminReferralHandler {
	return &AdminReferralHandler{mongo: m}
}

// GetSettings returns the current referral programme configuration.
// Falls back to hardcoded defaults if the document doesn't exist yet.
//
// GET /api/v1/admin/referral/settings
func (h *AdminReferralHandler) GetSettings(c *fiber.Ctx) error {
	settings, err := loadReferralSettings(h.mongo, c.Context())
	if err != nil {
		return err
	}
	return c.JSON(settings)
}

// UpdateSettings overwrites the settings document (full replace semantics).
//
// PUT /api/v1/admin/referral/settings
func (h *AdminReferralHandler) UpdateSettings(c *fiber.Ctx) error {
	var req models.ReferralSettings
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	req.ID = "global"
	req.UpdatedAt = time.Now().UTC()

	if req.FirstCommissionAmount < 0 || req.RecurringCommissionAmount < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "commission amounts must be >= 0")
	}
	if req.DiscountPercent < 0 || req.DiscountPercent > 100 {
		return fiber.NewError(fiber.StatusBadRequest, "discount_percent must be 0–100")
	}
	if req.DiscountDurationMonths < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "discount_duration_months must be >= 0")
	}
	if req.DefaultPayoutType != models.PayoutTypeManual && req.DefaultPayoutType != models.PayoutTypeCredit {
		req.DefaultPayoutType = models.PayoutTypeManual
	}

	_, err := h.mongo.DB.Collection("referral_settings").UpdateOne(
		c.Context(),
		bson.M{"_id": "global"},
		bson.M{"$set": req},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return err
	}
	return c.JSON(req)
}

// ListReferrals returns all referrals, newest first, with enriched tenant names.
//
// GET /api/v1/admin/referral/referrals
func (h *AdminReferralHandler) ListReferrals(c *fiber.Ctx) error {
	cur, err := h.mongo.DB.Collection("referrals").Find(
		c.Context(),
		bson.M{},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(500),
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

	// Enrich with referrer tenant names in one batch lookup.
	ids := make([]string, 0, len(referrals))
	seen := map[string]bool{}
	for _, r := range referrals {
		if !seen[r.ReferrerTenantID] {
			ids = append(ids, r.ReferrerTenantID)
			seen[r.ReferrerTenantID] = true
		}
	}
	nameMap := map[string]string{}
	if len(ids) > 0 {
		tCur, err := h.mongo.DB.Collection("tenants").Find(
			c.Context(),
			bson.M{"_id": bson.M{"$in": ids}},
			options.Find().SetProjection(bson.M{"_id": 1, "name": 1}),
		)
		if err == nil {
			defer tCur.Close(c.Context())
			var ts []struct {
				ID   string `bson:"_id"`
				Name string `bson:"name"`
			}
			_ = tCur.All(c.Context(), &ts)
			for _, t := range ts {
				nameMap[t.ID] = t.Name
			}
		}
	}

	type row struct {
		models.Referral
		ReferrerTenantName string `json:"referrer_tenant_name"`
	}
	out := make([]row, 0, len(referrals))
	for _, r := range referrals {
		out = append(out, row{Referral: r, ReferrerTenantName: nameMap[r.ReferrerTenantID]})
	}
	return c.JSON(out)
}

// ListWallets returns all wallets that have a non-zero balance, for the
// admin payout queue.
//
// GET /api/v1/admin/referral/wallets
func (h *AdminReferralHandler) ListWallets(c *fiber.Ctx) error {
	cur, err := h.mongo.DB.Collection("wallets").Find(
		c.Context(),
		bson.M{"balance": bson.M{"$gt": 0}},
		options.Find().SetSort(bson.D{{Key: "balance", Value: -1}}),
	)
	if err != nil {
		return err
	}
	defer cur.Close(c.Context())

	var wallets []models.Wallet
	if err := cur.All(c.Context(), &wallets); err != nil {
		return err
	}
	if wallets == nil {
		wallets = []models.Wallet{}
	}

	// Enrich with tenant names.
	ids := make([]string, 0, len(wallets))
	for _, w := range wallets {
		ids = append(ids, w.TenantID)
	}
	nameMap := map[string]string{}
	if len(ids) > 0 {
		tCur, _ := h.mongo.DB.Collection("tenants").Find(
			c.Context(),
			bson.M{"_id": bson.M{"$in": ids}},
			options.Find().SetProjection(bson.M{"_id": 1, "name": 1}),
		)
		if tCur != nil {
			defer tCur.Close(c.Context())
			var ts []struct {
				ID   string `bson:"_id"`
				Name string `bson:"name"`
			}
			_ = tCur.All(c.Context(), &ts)
			for _, t := range ts {
				nameMap[t.ID] = t.Name
			}
		}
	}

	type walletRow struct {
		models.Wallet
		TenantName string `json:"tenant_name"`
	}
	out := make([]walletRow, 0, len(wallets))
	for _, w := range wallets {
		out = append(out, walletRow{Wallet: w, TenantName: nameMap[w.TenantID]})
	}
	return c.JSON(out)
}

// MarkPayoutDone zeros the wallet balance for a tenant, recording the debit
// transaction. Called after the admin has sent PromptPay / bank transfer.
//
// POST /api/v1/admin/referral/wallets/:id/payout
func (h *AdminReferralHandler) MarkPayoutDone(c *fiber.Ctx) error {
	walletID := c.Params("id") // = tenant_id
	if walletID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "missing wallet id")
	}

	var wallet models.Wallet
	if err := h.mongo.DB.Collection("wallets").
		FindOne(c.Context(), bson.M{"_id": walletID}).Decode(&wallet); err == mongo.ErrNoDocuments {
		return fiber.NewError(fiber.StatusNotFound, "wallet not found")
	} else if err != nil {
		return err
	}
	if wallet.Balance <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "wallet balance is already zero")
	}

	amount := wallet.Balance
	now := time.Now().UTC()

	txn := models.WalletTransaction{
		ID:          uuid.NewString(),
		TenantID:    walletID,
		Type:        models.TxnTypePayout,
		Amount:      -amount,
		Description: fmt.Sprintf("Manual payout by admin — ฿%.2f", float64(amount)/100),
		CreatedAt:   now,
	}
	if _, err := h.mongo.DB.Collection("wallet_transactions").InsertOne(c.Context(), txn); err != nil {
		return err
	}
	if _, err := h.mongo.DB.Collection("wallets").UpdateOne(
		c.Context(),
		bson.M{"_id": walletID},
		bson.M{"$set": bson.M{"balance": 0, "updated_at": now}},
	); err != nil {
		return err
	}

	return c.JSON(fiber.Map{
		"ok":     true,
		"amount": amount,
	})
}

// UpdateWalletPayoutType lets the admin override the payout type for one wallet.
//
// PATCH /api/v1/admin/referral/wallets/:id
func (h *AdminReferralHandler) UpdateWalletPayoutType(c *fiber.Ctx) error {
	walletID := c.Params("id")
	var req struct {
		PayoutType string `json:"payout_type"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.PayoutType != models.PayoutTypeManual && req.PayoutType != models.PayoutTypeCredit {
		return fiber.NewError(fiber.StatusBadRequest, "payout_type must be 'manual' or 'credit'")
	}
	_, err := h.mongo.DB.Collection("wallets").UpdateOne(
		c.Context(),
		bson.M{"_id": walletID},
		bson.M{"$set": bson.M{"payout_type": req.PayoutType, "updated_at": time.Now().UTC()}},
	)
	if err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ListPayoutRequests returns all payout requests, optionally filtered by status.
//
// GET /api/v1/admin/referral/payout-requests?status=pending
func (h *AdminReferralHandler) ListPayoutRequests(c *fiber.Ctx) error {
	filter := bson.M{}
	if s := c.Query("status"); s != "" {
		filter["status"] = s
	}

	cur, err := h.mongo.DB.Collection("payout_requests").Find(
		c.Context(),
		filter,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(200),
	)
	if err != nil {
		return err
	}
	defer cur.Close(c.Context())

	var requests []models.PayoutRequest
	if err := cur.All(c.Context(), &requests); err != nil {
		return err
	}
	if requests == nil {
		requests = []models.PayoutRequest{}
	}

	// Enrich with tenant names.
	ids := make([]string, 0, len(requests))
	seen := map[string]bool{}
	for _, r := range requests {
		if !seen[r.TenantID] {
			ids = append(ids, r.TenantID)
			seen[r.TenantID] = true
		}
	}
	nameMap := map[string]string{}
	if len(ids) > 0 {
		tCur, err := h.mongo.DB.Collection("tenants").Find(
			c.Context(),
			bson.M{"_id": bson.M{"$in": ids}},
			options.Find().SetProjection(bson.M{"_id": 1, "name": 1}),
		)
		if err == nil {
			defer tCur.Close(c.Context())
			var ts []struct {
				ID   string `bson:"_id"`
				Name string `bson:"name"`
			}
			_ = tCur.All(c.Context(), &ts)
			for _, t := range ts {
				nameMap[t.ID] = t.Name
			}
		}
	}

	type row struct {
		models.PayoutRequest
		TenantName string `json:"tenant_name"`
	}
	out := make([]row, 0, len(requests))
	for _, r := range requests {
		out = append(out, row{PayoutRequest: r, TenantName: nameMap[r.TenantID]})
	}
	return c.JSON(out)
}

// ApprovePayoutRequest marks a pending payout request as approved.
// The wallet was already debited at submission time — this just confirms
// that the admin has sent the bank transfer.
//
// POST /api/v1/admin/referral/payout-requests/:id/approve
func (h *AdminReferralHandler) ApprovePayoutRequest(c *fiber.Ctx) error {
	reqID := c.Params("id")
	if reqID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "missing request id")
	}

	var req struct {
		AdminNote string `json:"admin_note"`
	}
	_ = c.BodyParser(&req)

	var pr models.PayoutRequest
	if err := h.mongo.DB.Collection("payout_requests").
		FindOne(c.Context(), bson.M{"_id": reqID}).Decode(&pr); err == mongo.ErrNoDocuments {
		return fiber.NewError(fiber.StatusNotFound, "payout request not found")
	} else if err != nil {
		return err
	}
	if pr.Status != models.PayoutRequestStatusPending {
		return fiber.NewError(fiber.StatusBadRequest, "request is not in pending status")
	}

	now := time.Now().UTC()
	adminUserID := middleware.UserID(c)

	_, err := h.mongo.DB.Collection("payout_requests").UpdateOne(
		c.Context(),
		bson.M{"_id": reqID},
		bson.M{"$set": bson.M{
			"status":      models.PayoutRequestStatusApproved,
			"approved_by": adminUserID,
			"approved_at": now,
			"admin_note":  req.AdminNote,
			"updated_at":  now,
		}},
	)
	if err != nil {
		return err
	}

	return c.JSON(fiber.Map{
		"ok":     true,
		"amount": pr.Amount,
	})
}

// RejectPayoutRequest marks a pending payout request as rejected and
// refunds the amount back to the tenant's wallet.
//
// POST /api/v1/admin/referral/payout-requests/:id/reject
func (h *AdminReferralHandler) RejectPayoutRequest(c *fiber.Ctx) error {
	reqID := c.Params("id")
	if reqID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "missing request id")
	}

	var req struct {
		AdminNote string `json:"admin_note"`
	}
	_ = c.BodyParser(&req)

	var pr models.PayoutRequest
	if err := h.mongo.DB.Collection("payout_requests").
		FindOne(c.Context(), bson.M{"_id": reqID}).Decode(&pr); err == mongo.ErrNoDocuments {
		return fiber.NewError(fiber.StatusNotFound, "payout request not found")
	} else if err != nil {
		return err
	}
	if pr.Status != models.PayoutRequestStatusPending {
		return fiber.NewError(fiber.StatusBadRequest, "request is not in pending status")
	}

	now := time.Now().UTC()
	adminUserID := middleware.UserID(c)

	// Refund the amount back to the wallet.
	if _, err := h.mongo.DB.Collection("wallets").UpdateOne(
		c.Context(),
		bson.M{"_id": pr.TenantID},
		bson.M{
			"$inc": bson.M{"balance": pr.Amount},
			"$set": bson.M{"tenant_id": pr.TenantID, "updated_at": now},
			"$setOnInsert": bson.M{"payout_type": models.PayoutTypeManual},
		},
		options.Update().SetUpsert(true),
	); err != nil {
		return err
	}

	// Record refund transaction.
	txn := models.WalletTransaction{
		ID:          uuid.NewString(),
		TenantID:    pr.TenantID,
		Type:        models.TxnTypeCommission, // reuse as a credit entry
		Amount:      pr.Amount,
		Description: fmt.Sprintf("คืนเงินจากคำขอเบิกที่ถูกปฏิเสธ — ฿%.2f", float64(pr.Amount)/100),
		CreatedAt:   now,
	}
	if _, err := h.mongo.DB.Collection("wallet_transactions").InsertOne(c.Context(), txn); err != nil {
		return err
	}

	// Update payout request status.
	_, err := h.mongo.DB.Collection("payout_requests").UpdateOne(
		c.Context(),
		bson.M{"_id": reqID},
		bson.M{"$set": bson.M{
			"status":      models.PayoutRequestStatusRejected,
			"approved_by": adminUserID,
			"approved_at": now,
			"admin_note":  req.AdminNote,
			"updated_at":  now,
		}},
	)
	if err != nil {
		return err
	}

	return c.JSON(fiber.Map{"ok": true, "refunded": pr.Amount})
}

// ── shared helper ─────────────────────────────────────────────────────────────

// loadReferralSettings fetches the settings doc, falling back to defaults.
func loadReferralSettings(m *db.Mongo, ctx context.Context) (models.ReferralSettings, error) {
	var s models.ReferralSettings
	err := m.DB.Collection("referral_settings").
		FindOne(ctx, bson.M{"_id": "global"}).Decode(&s)
	if err == mongo.ErrNoDocuments {
		return models.DefaultReferralSettings(), nil
	}
	return s, err
}
