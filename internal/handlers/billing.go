package handlers

// Stripe billing — tenant-facing self-service.
//
// Endpoints:
//
//   GET    /api/v1/billing                          — current plan, subscription & usage
//   POST   /api/v1/billing/checkout-session         — subscribe / upgrade (month or year)
//   POST   /api/v1/billing/portal-session           — Stripe Customer Portal
//   GET    /api/v1/billing/payment-methods          — list saved cards
//   DELETE /api/v1/billing/payment-methods/:id      — detach a card
//
// Stripe Customer is created lazily on first Checkout. The webhook
// (webhook_stripe.go) is the source of truth for subscription state.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stripe/stripe-go/v79"
	billingportal "github.com/stripe/stripe-go/v79/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v79/checkout/session"
	stripecoupon "github.com/stripe/stripe-go/v79/coupon"
	stripecustomer "github.com/stripe/stripe-go/v79/customer"
	stripeinvoice "github.com/stripe/stripe-go/v79/invoice"
	stripepaymentmethod "github.com/stripe/stripe-go/v79/paymentmethod"
	stripesub "github.com/stripe/stripe-go/v79/subscription"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type BillingHandler struct {
	mongo *db.Mongo
	cfg   *config.Config
}

func NewBillingHandler(m *db.Mongo, cfg *config.Config) *BillingHandler {
	if cfg.StripeSecretKey != "" {
		stripe.Key = cfg.StripeSecretKey
	}
	return &BillingHandler{mongo: m, cfg: cfg}
}

// ensureCustomer makes sure the tenant has a Stripe Customer object,
// creating one on first call. Idempotent.
func (h *BillingHandler) ensureCustomer(c *fiber.Ctx, t *models.Tenant) (string, error) {
	if t.StripeCustomerID != "" {
		return t.StripeCustomerID, nil
	}
	cust, err := stripecustomer.New(&stripe.CustomerParams{
		Name:  stripe.String(t.Name),
		Email: stripe.String(middleware.Email(c)),
		Metadata: map[string]string{
			"tenant_id": t.ID,
		},
	})
	if err != nil {
		return "", err
	}
	_, err = h.mongo.DB.Collection("tenants").UpdateOne(c.Context(),
		bson.M{"_id": t.ID},
		bson.M{"$set": bson.M{"stripe_customer_id": cust.ID}})
	if err != nil {
		return "", err
	}
	return cust.ID, nil
}

// ── Checkout ──────────────────────────────────────────────────────────

type checkoutReq struct {
	Plan     string `json:"plan"`     // "starter" | "growth" | "pro" | "enterprise"
	Interval string `json:"interval"` // "month" (default) | "year"
}

// POST /api/v1/billing/checkout-session
//
// Accepts { plan, interval }. interval defaults to "month".
// Picks the matching Stripe price from the plan document (no env vars).
func (h *BillingHandler) CreateCheckoutSession(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing not configured")
	}
	tid := middleware.TenantID(c)

	var req checkoutReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.Interval == "" {
		req.Interval = "month"
	}
	if req.Interval != "month" && req.Interval != "year" {
		return fiber.NewError(fiber.StatusBadRequest, "interval must be \"month\" or \"year\"")
	}

	// Load plan from DB — price IDs live here, not in env.
	var plan models.Plan
	if err := h.mongo.DB.Collection("plans").
		FindOne(c.Context(), bson.M{"_id": req.Plan}).Decode(&plan); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "plan not found: "+req.Plan)
	}

	// Pick monthly or yearly price ID.
	priceID := plan.StripePriceID
	if req.Interval == "year" {
		if plan.StripePriceIDYearly == "" {
			return fiber.NewError(fiber.StatusBadRequest,
				"plan \""+req.Plan+"\" has no yearly price — set it in Admin → Plans")
		}
		priceID = plan.StripePriceIDYearly
	}
	if priceID == "" {
		return fiber.NewError(fiber.StatusBadRequest,
			"plan \""+req.Plan+"\" has no Stripe price ID — set it in Admin → Plans")
	}

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	customerID, err := h.ensureCustomer(c, &t)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe customer create: "+err.Error())
	}

	params := &stripe.CheckoutSessionParams{
		Mode:                stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:            stripe.String(customerID),
		AllowPromotionCodes: stripe.Bool(true),
		ClientReferenceID:   stripe.String(t.ID),
		// Session-level metadata — readable directly from checkout.session.completed
		// without expanding the subscription object.
		Params: stripe.Params{
			Metadata: map[string]string{
				"tenant_id": t.ID,
				"plan":      req.Plan,
				"interval":  req.Interval,
			},
		},
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			Price:    stripe.String(priceID),
			Quantity: stripe.Int64(1),
		}},
		SuccessURL: stripe.String(h.cfg.BillingReturnURL + "?status=success&session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:  stripe.String(h.cfg.BillingReturnURL + "?status=cancel"),
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{
			// Also set on the subscription so syncSubscription() can read it
			// from customer.subscription.created / .updated events.
			Metadata: map[string]string{
				"tenant_id": t.ID,
				"plan":      req.Plan,
				"interval":  req.Interval,
			},
		},
	}
	// Apply referral discount when the tenant has a valid signup discount.
	// Discounts and AllowPromotionCodes are mutually exclusive in Stripe, so
	// we clear AllowPromotionCodes when we inject an explicit discount coupon.
	if t.ReferralDiscountExpiresAt != nil && time.Now().UTC().Before(*t.ReferralDiscountExpiresAt) {
		if settings, serr := loadReferralSettings(h.mongo, c.Context()); serr == nil && settings.DiscountPercent > 0 {
			if couponID, cerr := ensureReferralCoupon(settings.DiscountPercent, settings.DiscountDurationMonths); cerr == nil {
				params.Discounts = []*stripe.CheckoutSessionDiscountParams{
					{Coupon: stripe.String(couponID)},
				}
				params.AllowPromotionCodes = nil // cannot coexist with Discounts
			}
		}
	}

	sess, err := checkoutsession.New(params)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe checkout: "+err.Error())
	}
	return c.JSON(fiber.Map{"url": sess.URL})
}

// ── PromptPay checkout ────────────────────────────────────────────────

type promptPayCheckoutReq struct {
	Plan     string `json:"plan"`
	Interval string `json:"interval"` // "month" | "year"
}

// POST /api/v1/billing/promptpay-checkout
//
// Creates a one-time Stripe Checkout session with payment_method_types=["promptpay"].
// PromptPay does NOT support recurring subscriptions — this mints a single
// charge for one billing period. On success, SyncCheckoutSession grants plan
// access for that period; cancel_at_period_end=true ensures it auto-expires
// via the nightly sweep if the tenant doesn't renew.
func (h *BillingHandler) CreatePromptPayCheckout(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing not configured")
	}
	tid := middleware.TenantID(c)

	var req promptPayCheckoutReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.Interval == "" {
		req.Interval = "month"
	}
	if req.Interval != "month" && req.Interval != "year" {
		return fiber.NewError(fiber.StatusBadRequest, "interval must be \"month\" or \"year\"")
	}

	var plan models.Plan
	if err := h.mongo.DB.Collection("plans").
		FindOne(c.Context(), bson.M{"_id": req.Plan}).Decode(&plan); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "plan not found: "+req.Plan)
	}
	if plan.Price == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "cannot pay for a free plan")
	}

	// Compute amount in satang (THB × 100). Yearly falls back to monthly
	// price × 12 when no dedicated yearly price is set.
	var amountSatang int64
	var periodLabel string
	if req.Interval == "year" {
		yp := plan.YearlyPrice
		if yp == 0 {
			yp = plan.Price * 12
		}
		amountSatang = int64(yp * 100)
		periodLabel = "1 ปี / 1 Year"
	} else {
		amountSatang = int64(plan.Price * 100)
		periodLabel = "1 เดือน / 1 Month"
	}

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	customerID, err := h.ensureCustomer(c, &t)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe customer create: "+err.Error())
	}

	params := &stripe.CheckoutSessionParams{
		Mode:               stripe.String(string(stripe.CheckoutSessionModePayment)),
		Customer:           stripe.String(customerID),
		PaymentMethodTypes: stripe.StringSlice([]string{"promptpay"}),
		ClientReferenceID:  stripe.String(t.ID),
		Params: stripe.Params{
			Metadata: map[string]string{
				"tenant_id": t.ID,
				"plan":      req.Plan,
				"interval":  req.Interval,
				"source":    "promptpay",
			},
		},
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String("thb"),
				UnitAmount: stripe.Int64(amountSatang),
				ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
					Name: stripe.String(plan.DisplayName + " — " + periodLabel),
				},
			},
			Quantity: stripe.Int64(1),
		}},
		SuccessURL: stripe.String(h.cfg.BillingReturnURL + "?status=success&session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:  stripe.String(h.cfg.BillingReturnURL + "?status=cancel"),
	}
	sess, err := checkoutsession.New(params)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe promptpay checkout: "+err.Error())
	}
	return c.JSON(fiber.Map{"url": sess.URL})
}

// ── Portal ────────────────────────────────────────────────────────────

// POST /api/v1/billing/portal-session
//
// Returns { url } for the Stripe Customer Portal (card management,
// plan changes, invoice history, cancellation).
func (h *BillingHandler) CreatePortalSession(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing not configured")
	}
	tid := middleware.TenantID(c)

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	// Create a Stripe customer on the fly if one doesn't exist yet.
	// This lets users manage cards / view invoices even before their first
	// checkout (e.g. an admin manually assigned them a paid plan).
	customerID, err := h.ensureCustomer(c, &t)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe customer: "+err.Error())
	}

	sess, err := billingportal.New(&stripe.BillingPortalSessionParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(h.cfg.BillingReturnURL),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe portal: "+err.Error())
	}
	return c.JSON(fiber.Map{"url": sess.URL})
}

// ── Payment methods ───────────────────────────────────────────────────

type cardView struct {
	ID        string `json:"id"`
	Brand     string `json:"brand"`
	Last4     string `json:"last4"`
	ExpMonth  int64  `json:"exp_month"`
	ExpYear   int64  `json:"exp_year"`
	IsDefault bool   `json:"is_default"`
}

// GET /api/v1/billing/payment-methods
//
// Lists saved cards for the tenant's Stripe customer. Deduplicates by card
// fingerprint so that multiple checkout attempts with the same physical card
// only appear once — the most-recent PM object wins.
func (h *BillingHandler) ListPaymentMethods(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return c.JSON(fiber.Map{"payment_methods": []cardView{}})
	}
	tid := middleware.TenantID(c)

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	if t.StripeCustomerID == "" {
		return c.JSON(fiber.Map{"payment_methods": []cardView{}})
	}

	// Find the default PM: check subscription first, fall back to customer default.
	defaultPMID := ""
	cust, err := stripecustomer.Get(t.StripeCustomerID, &stripe.CustomerParams{
		Params: stripe.Params{Expand: []*string{
			stripe.String("invoice_settings.default_payment_method"),
		}},
	})
	if err == nil && cust.InvoiceSettings != nil && cust.InvoiceSettings.DefaultPaymentMethod != nil {
		defaultPMID = cust.InvoiceSettings.DefaultPaymentMethod.ID
	}
	// Also check the active subscription's default PM.
	if defaultPMID == "" && t.StripeSubscriptionID != "" {
		sub, serr := stripesub.Get(t.StripeSubscriptionID, &stripe.SubscriptionParams{
			Params: stripe.Params{Expand: []*string{stripe.String("default_payment_method")}},
		})
		if serr == nil && sub.DefaultPaymentMethod != nil {
			defaultPMID = sub.DefaultPaymentMethod.ID
		}
	}

	// List all card PMs, newest first (Stripe returns them in reverse-creation order).
	iter := stripepaymentmethod.List(&stripe.PaymentMethodListParams{
		Customer: stripe.String(t.StripeCustomerID),
		Type:     stripe.String(string(stripe.PaymentMethodTypeCard)),
	})

	// Deduplicate by card fingerprint — same physical card number regardless of
	// how many PM objects Stripe created across multiple checkout attempts.
	seen := map[string]bool{}
	var cards []cardView
	for iter.Next() {
		pm := iter.PaymentMethod()
		if pm.Card == nil {
			continue
		}
		fp := pm.Card.Fingerprint
		if fp != "" && seen[fp] {
			continue // duplicate physical card — skip older PM objects
		}
		if fp != "" {
			seen[fp] = true
		}
		cards = append(cards, cardView{
			ID:        pm.ID,
			Brand:     string(pm.Card.Brand),
			Last4:     pm.Card.Last4,
			ExpMonth:  pm.Card.ExpMonth,
			ExpYear:   pm.Card.ExpYear,
			IsDefault: pm.ID == defaultPMID || (defaultPMID == "" && len(cards) == 0),
		})
	}
	if err := iter.Err(); err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe list: "+err.Error())
	}

	if cards == nil {
		cards = []cardView{}
	}
	return c.JSON(fiber.Map{"payment_methods": cards})
}

// DELETE /api/v1/billing/payment-methods/:id
//
// Detaches a card from the Stripe customer. The tenant must own the
// payment method (we verify the customer matches before detaching).
func (h *BillingHandler) RemovePaymentMethod(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing not configured")
	}
	pmID := c.Params("id")
	if pmID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "payment method id required")
	}
	tid := middleware.TenantID(c)

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}

	// Verify ownership: fetch the PM and confirm it belongs to this customer.
	pm, err := stripepaymentmethod.Get(pmID, nil)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "payment method not found")
	}
	if pm.Customer == nil || pm.Customer.ID != t.StripeCustomerID {
		return fiber.NewError(fiber.StatusForbidden, "payment method does not belong to this account")
	}

	if _, err := stripepaymentmethod.Detach(pmID, nil); err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "detach failed: "+err.Error())
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Cancel / Reactivate ───────────────────────────────────────────────

// POST /api/v1/billing/cancel
//
// Schedules the subscription to cancel at the end of the current billing
// period (cancel_at_period_end=true). The tenant keeps full plan access
// until that date — no immediate downgrade.
func (h *BillingHandler) CancelSubscription(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing not configured")
	}
	tid := middleware.TenantID(c)

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	if t.StripeSubscriptionID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "no active subscription")
	}

	_, err := stripesub.Update(t.StripeSubscriptionID, &stripe.SubscriptionParams{
		CancelAtPeriodEnd: stripe.Bool(true),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe cancel: "+err.Error())
	}
	// The webhook (customer.subscription.updated) will sync the local state;
	// optimistically update cancel_at_period_end here so the UI reflects it
	// immediately without waiting for webhook delivery.
	_, _ = h.mongo.DB.Collection("tenants").UpdateOne(c.Context(),
		bson.M{"_id": tid},
		bson.M{"$set": bson.M{
			"subscription.cancel_at_period_end": true,
			"subscription.updated_at":           time.Now().UTC(),
		}},
	)
	return c.SendStatus(fiber.StatusNoContent)
}

// POST /api/v1/billing/reactivate
//
// Removes the scheduled cancellation (cancel_at_period_end=false) so the
// subscription renews normally at the next billing date.
func (h *BillingHandler) ReactivateSubscription(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing not configured")
	}
	tid := middleware.TenantID(c)

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	if t.StripeSubscriptionID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "no active subscription")
	}

	_, err := stripesub.Update(t.StripeSubscriptionID, &stripe.SubscriptionParams{
		CancelAtPeriodEnd: stripe.Bool(false),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe reactivate: "+err.Error())
	}
	_, _ = h.mongo.DB.Collection("tenants").UpdateOne(c.Context(),
		bson.M{"_id": tid},
		bson.M{"$set": bson.M{
			"subscription.cancel_at_period_end": false,
			"subscription.updated_at":           time.Now().UTC(),
		}},
	)
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Subscription expiry sweep ─────────────────────────────────────────

// ExpireSubscriptions downgrades tenants whose paid access has lapsed.
//
// Two cases are handled:
//
//  1. cancel_at_period_end=true and current_period_end has passed.
//  2. status=trialing and trial_ends_at has passed.
func ExpireSubscriptions(ctx context.Context, mongo *db.Mongo) (int, error) {
	now := time.Now().UTC()

	filter := bson.M{
		"plan": bson.M{"$ne": "free"},
		"$or": []bson.M{
			{
				"subscription.cancel_at_period_end": true,
				"subscription.current_period_end":   bson.M{"$lte": now},
			},
			{
				"subscription.status":        "trialing",
				"subscription.trial_ends_at": bson.M{"$lte": now},
			},
		},
	}
	update := bson.M{
		"$set": bson.M{
			"plan":                              "free",
			"stripe_subscription_id":            "",
			"subscription.status":               "canceled",
			"subscription.cancel_at_period_end": false,
			"subscription.canceled_at":          now,
			"subscription.updated_at":           now,
		},
	}
	res, err := mongo.DB.Collection("tenants").UpdateMany(ctx, filter, update)
	if err != nil {
		return 0, err
	}
	return int(res.ModifiedCount), nil
}

// ── Sync checkout session ─────────────────────────────────────────────

// POST /api/v1/billing/sync-session
//
// Called by the frontend immediately after Stripe redirects back with
// ?session_id=.... Fetches the checkout session from Stripe (with the
// subscription expanded) and writes the plan + subscription to MongoDB in
// one shot. This is the reliable "pull" path — it works even when the
// webhook hasn't fired yet or isn't configured at all.
func (h *BillingHandler) SyncCheckoutSession(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing not configured")
	}
	tid := middleware.TenantID(c)

	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := c.BodyParser(&req); err != nil || req.SessionID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "session_id required")
	}

	// Fetch the session with its subscription and payment_intent fully expanded
	// so we have all metadata + period dates + receipt URL without extra API calls.
	sess, err := checkoutsession.Get(req.SessionID, &stripe.CheckoutSessionParams{
		Params: stripe.Params{Expand: []*string{
			stripe.String("subscription"),
			stripe.String("payment_intent.latest_charge"),
		}},
	})
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe fetch session: "+err.Error())
	}

	// Guard: the session must belong to this tenant.
	if sess.ClientReferenceID != tid {
		return fiber.NewError(fiber.StatusForbidden, "session does not belong to this account")
	}

	ctx := c.Context()

	// Resolve the plan slug from session metadata first (most reliable).
	planSlug := sess.Metadata["plan"]

	// ── PromptPay path: mode = "payment" (no subscription object) ────────────
	if sess.Mode == stripe.CheckoutSessionModePayment {
		if sess.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
			return fiber.NewError(fiber.StatusBadRequest, "payment not yet confirmed — please wait a moment and try again")
		}

		// Determine the access period from metadata.
		interval := sess.Metadata["interval"]
		now := time.Now().UTC()
		var periodEnd time.Time
		if interval == "year" {
			periodEnd = now.AddDate(1, 0, 0)
		} else {
			periodEnd = now.AddDate(0, 1, 0)
		}

		// Preserve any existing admin notes.
		var existing models.Tenant
		var adminNotes string
		if err := h.mongo.DB.Collection("tenants").FindOne(ctx, bson.M{"_id": tid}).Decode(&existing); err == nil && existing.Subscription != nil {
			adminNotes = existing.Subscription.AdminNotes
		}

		sub := &models.Subscription{
			Status:            models.SubStatusActive,
			CurrentPeriodEnd:  &periodEnd,
			CancelAtPeriodEnd: true, // auto-expires unless tenant renews
			AdminNotes:        adminNotes,
			UpdatedAt:         now,
		}
		updates := bson.M{
			"subscription":           sub,
			"stripe_subscription_id": "", // PromptPay is not a recurring Stripe subscription
		}
		if planSlug != "" {
			updates["plan"] = planSlug
		}
		_, _ = h.mongo.DB.Collection("tenants").UpdateOne(ctx,
			bson.M{"_id": tid},
			bson.M{"$set": updates},
		)

		// ── Upsert Payment record (idempotent: session ID is _id) ────────────────
		var planDoc models.Plan
		_ = h.mongo.DB.Collection("plans").FindOne(ctx, bson.M{"_id": planSlug}).Decode(&planDoc)
		planName := planSlug
		if planDoc.DisplayName != "" {
			planName = planDoc.DisplayName
		}
		desc := planName
		if interval == "year" {
			desc += " — 1 Year"
		} else {
			desc += " — 1 Month"
		}
		receiptURL := ""
		if sess.PaymentIntent != nil && sess.PaymentIntent.LatestCharge != nil {
			receiptURL = sess.PaymentIntent.LatestCharge.ReceiptURL
		}
		payment := models.Payment{
			ID:          req.SessionID,
			TenantID:    tid,
			Source:      "promptpay",
			Plan:        planSlug,
			DisplayName: planName,
			Interval:    interval,
			Amount:      sess.AmountTotal,
			Currency:    string(sess.Currency),
			Status:      "paid",
			Description: desc,
			PeriodStart: now,
			PeriodEnd:   periodEnd,
			ReceiptURL:  receiptURL,
			CreatedAt:   now,
		}
		_, _ = h.mongo.DB.Collection("payments").ReplaceOne(ctx,
			bson.M{"_id": payment.ID},
			payment,
			options.Replace().SetUpsert(true),
		)

		return c.SendStatus(fiber.StatusNoContent)
	}

	// ── Card / subscription path: mode = "subscription" ──────────────────────
	if sess.Subscription == nil {
		return fiber.NewError(fiber.StatusBadRequest, "not a subscription checkout session")
	}
	if planSlug == "" {
		planSlug = sess.Subscription.Metadata["plan"]
	}

	// Write the full subscription state (status, period end, etc.).
	if err := syncSubscription(ctx, h.mongo, tid, sess.Subscription); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "sync subscription: "+err.Error())
	}

	// Update the plan slug and subscription ID.
	extra := bson.M{"stripe_subscription_id": sess.Subscription.ID}
	if planSlug != "" {
		extra["plan"] = planSlug
	}
	_, _ = h.mongo.DB.Collection("tenants").UpdateOne(ctx,
		bson.M{"_id": tid},
		bson.M{"$set": extra},
	)

	// Promote the subscription's payment method to the customer's invoice
	// default so it shows up in the Stripe Customer Portal.
	if sess.Subscription.DefaultPaymentMethod != nil && sess.Subscription.DefaultPaymentMethod.ID != "" {
		_, _ = stripecustomer.Update(sess.Customer.ID, &stripe.CustomerParams{
			InvoiceSettings: &stripe.CustomerInvoiceSettingsParams{
				DefaultPaymentMethod: stripe.String(sess.Subscription.DefaultPaymentMethod.ID),
			},
		})
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// ── Invoices ──────────────────────────────────────────────────────────

type invoiceView struct {
	ID          string `json:"id"`
	Number      string `json:"number"`
	AmountPaid  int64  `json:"amount_paid"`  // in smallest unit (satang / cent)
	Currency    string `json:"currency"`
	Status      string `json:"status"`       // paid | open | void | uncollectible
	Source      string `json:"source"`       // "invoice" | "promptpay"
	Description string `json:"description"`  // e.g. "Starter — 1 Month"
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	InvoiceURL  string `json:"invoice_url"`  // hosted invoice or receipt page
	PDFURL      string `json:"pdf_url"`
	CreatedAt   string `json:"created_at"`
	CreatedUnix int64  `json:"created_unix"` // for sorting
}

// GET /api/v1/billing/invoices
//
// Returns the last 24 payment records for the tenant, newest first.
// Two sources are merged:
//   - Stripe Invoices (card / subscription renewals)
//   - MongoDB payments collection (PromptPay one-time payments, stored on checkout)
func (h *BillingHandler) ListInvoices(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return c.JSON(fiber.Map{"invoices": []invoiceView{}})
	}
	tid := middleware.TenantID(c)

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	if t.StripeCustomerID == "" {
		return c.JSON(fiber.Map{"invoices": []invoiceView{}})
	}

	var (
		invoices []invoiceView
		mu       sync.Mutex
		wg       sync.WaitGroup
	)

	// ── Stripe Invoices (card / subscription) ────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		limit := int64(24)
		iter := stripeinvoice.List(&stripe.InvoiceListParams{
			Customer:   stripe.String(t.StripeCustomerID),
			ListParams: stripe.ListParams{Limit: &limit},
		})
		for iter.Next() {
			inv := iter.Invoice()
			if inv.Status == stripe.InvoiceStatusDraft {
				continue
			}
			v := invoiceView{
				ID:          inv.ID,
				Number:      inv.Number,
				AmountPaid:  inv.AmountPaid,
				Currency:    string(inv.Currency),
				Status:      string(inv.Status),
				Source:      "invoice",
				InvoiceURL:  inv.HostedInvoiceURL,
				PDFURL:      inv.InvoicePDF,
				CreatedAt:   time.Unix(inv.Created, 0).UTC().Format("2006-01-02"),
				CreatedUnix: inv.Created,
			}
			if inv.Lines != nil && len(inv.Lines.Data) > 0 && inv.Lines.Data[0].Period != nil {
				v.PeriodStart = time.Unix(inv.Lines.Data[0].Period.Start, 0).UTC().Format("2006-01-02")
				v.PeriodEnd = time.Unix(inv.Lines.Data[0].Period.End, 0).UTC().Format("2006-01-02")
			}
			mu.Lock()
			invoices = append(invoices, v)
			mu.Unlock()
		}
	}()

	// ── PromptPay payments from MongoDB ──────────────────────────────────────
	// PromptPay records are stored locally (payments collection) rather than
	// fetched from Stripe, because Stripe payment-mode sessions don't create
	// Invoices and the PaymentIntent list doesn't carry our metadata reliably.
	wg.Add(1)
	go func() {
		defer wg.Done()
		cur, err := h.mongo.DB.Collection("payments").Find(c.Context(),
			bson.M{"tenant_id": tid, "source": "promptpay"},
			options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(24),
		)
		if err != nil {
			return
		}
		defer cur.Close(c.Context())
		for cur.Next(c.Context()) {
			var p models.Payment
			if err := cur.Decode(&p); err != nil {
				continue
			}
			v := invoiceView{
				ID:          p.ID,
				Number:      "",
				AmountPaid:  p.Amount,
				Currency:    p.Currency,
				Status:      p.Status,
				Source:      "promptpay",
				Description: p.Description,
				PeriodStart: p.PeriodStart.UTC().Format("2006-01-02"),
				PeriodEnd:   p.PeriodEnd.UTC().Format("2006-01-02"),
				InvoiceURL:  p.ReceiptURL,
				CreatedAt:   p.CreatedAt.UTC().Format("2006-01-02"),
				CreatedUnix: p.CreatedAt.Unix(),
			}
			mu.Lock()
			invoices = append(invoices, v)
			mu.Unlock()
		}
	}()

	wg.Wait()

	// Sort all records newest-first, then cap at 24.
	sort.Slice(invoices, func(i, j int) bool {
		return invoices[i].CreatedUnix > invoices[j].CreatedUnix
	})
	if len(invoices) > 24 {
		invoices = invoices[:24]
	}
	if invoices == nil {
		invoices = []invoiceView{}
	}
	return c.JSON(fiber.Map{"invoices": invoices})
}

// ── Referral coupon helper ────────────────────────────────────────────

// ensureReferralCoupon finds or creates a Stripe coupon for the referral
// signup discount. Uses a deterministic coupon ID so we reuse one object
// across all checkout sessions rather than creating a new coupon every time.
func ensureReferralCoupon(discountPercent, durationMonths int) (string, error) {
	couponID := fmt.Sprintf("REFERRAL_%dPCT_%dMO", discountPercent, durationMonths)

	// Try to fetch an existing valid coupon first.
	existing, err := stripecoupon.Get(couponID, nil)
	if err == nil && existing.Valid {
		return couponID, nil
	}

	// Create the coupon — repeating for N months, or forever if months=0.
	dur := stripe.CouponDurationRepeating
	if durationMonths <= 0 {
		dur = stripe.CouponDurationForever
	}
	cp := &stripe.CouponParams{
		ID:         stripe.String(couponID),
		PercentOff: stripe.Float64(float64(discountPercent)),
		Duration:   stripe.String(string(dur)),
		Name:       stripe.String(fmt.Sprintf("Referral signup discount — %d%% off", discountPercent)),
	}
	if durationMonths > 0 {
		cp.DurationInMonths = stripe.Int64(int64(durationMonths))
	}
	c, err := stripecoupon.New(cp)
	if err != nil {
		// Race condition: coupon may have been created by a concurrent request.
		if existing2, e2 := stripecoupon.Get(couponID, nil); e2 == nil {
			return existing2.ID, nil
		}
		return "", fmt.Errorf("create referral coupon: %w", err)
	}
	return c.ID, nil
}

// ── Billing info ──────────────────────────────────────────────────────

type BillingUsage struct {
	Members           int `json:"members"`
	Channels          int `json:"channels"`
	MessagesThisMonth int `json:"messages_this_month"`
}

type BillingInfoResponse struct {
	Plan              *models.Plan         `json:"plan"`
	Subscription      *models.Subscription `json:"subscription,omitempty"`
	HasSubscription   bool                 `json:"has_subscription"`
	HasStripeCustomer bool                 `json:"has_stripe_customer"`
	Usage             BillingUsage         `json:"usage"`
}

// GET /api/v1/billing
func (h *BillingHandler) GetInfo(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	ctx := c.Context()

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(ctx, bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}

	var plan models.Plan
	if err := h.mongo.DB.Collection("plans").
		FindOne(ctx, bson.M{"_id": t.Plan}).Decode(&plan); err != nil {
		plan = models.Plan{ID: t.Plan, DisplayName: t.Plan}
	}

	memberCount, _ := h.mongo.DB.Collection("users").
		CountDocuments(ctx, bson.M{"tenant_id": tid})
	chanCount, _ := h.mongo.DB.Collection("channel_connections").
		CountDocuments(ctx, bson.M{"tenant_id": tid})

	startOfMonth := time.Date(
		time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC,
	)
	// Count only AI-generated replies, not inbound customer messages or
	// human-team messages. Roles: "ai" (sent) | "suggestion" (drafted, not sent).
	// We count only "ai" — suggestions that were never sent shouldn't count.
	msgCount, _ := h.mongo.DB.Collection("messages").
		CountDocuments(ctx, bson.M{
			"tenant_id":  tid,
			"role":       "ai",
			"created_at": bson.M{"$gte": startOfMonth},
		})

	return c.JSON(BillingInfoResponse{
		Plan:              &plan,
		Subscription:      t.Subscription,
		HasSubscription:   t.StripeSubscriptionID != "",
		HasStripeCustomer: t.StripeCustomerID != "",
		Usage: BillingUsage{
			Members:           int(memberCount),
			Channels:          int(chanCount),
			MessagesThisMonth: int(msgCount),
		},
	})
}
