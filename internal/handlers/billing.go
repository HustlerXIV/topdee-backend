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
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stripe/stripe-go/v79"
	billingportal "github.com/stripe/stripe-go/v79/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v79/checkout/session"
	stripecustomer "github.com/stripe/stripe-go/v79/customer"
	stripepaymentmethod "github.com/stripe/stripe-go/v79/paymentmethod"
	stripesub "github.com/stripe/stripe-go/v79/subscription"
	"go.mongodb.org/mongo-driver/bson"

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
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			Price:    stripe.String(priceID),
			Quantity: stripe.Int64(1),
		}},
		SuccessURL: stripe.String(h.cfg.BillingReturnURL + "?status=success&session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:  stripe.String(h.cfg.BillingReturnURL + "?status=cancel"),
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{
			Metadata: map[string]string{
				"tenant_id": t.ID,
				"plan":      req.Plan,
				"interval":  req.Interval,
			},
		},
	}
	sess, err := checkoutsession.New(params)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe checkout: "+err.Error())
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
	if t.StripeCustomerID == "" {
		return fiber.NewError(fiber.StatusBadRequest,
			"no Stripe customer yet — start a checkout session first")
	}

	sess, err := billingportal.New(&stripe.BillingPortalSessionParams{
		Customer:  stripe.String(t.StripeCustomerID),
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
// Lists all saved cards for the tenant's Stripe customer, marking
// which one is the default for their active subscription (or the
// customer's invoice default if no subscription exists).
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

	// Find which payment method is the default (subscription → customer fallback).
	defaultPMID := ""
	cust, err := stripecustomer.Get(t.StripeCustomerID, &stripe.CustomerParams{
		Params: stripe.Params{Expand: []*string{
			stripe.String("invoice_settings.default_payment_method"),
		}},
	})
	if err == nil &&
		cust.InvoiceSettings != nil &&
		cust.InvoiceSettings.DefaultPaymentMethod != nil {
		defaultPMID = cust.InvoiceSettings.DefaultPaymentMethod.ID
	}

	// List all card payment methods for this customer.
	iter := stripepaymentmethod.List(&stripe.PaymentMethodListParams{
		Customer: stripe.String(t.StripeCustomerID),
		Type:     stripe.String(string(stripe.PaymentMethodTypeCard)),
	})

	var cards []cardView
	for iter.Next() {
		pm := iter.PaymentMethod()
		if pm.Card == nil {
			continue
		}
		cards = append(cards, cardView{
			ID:        pm.ID,
			Brand:     string(pm.Card.Brand),
			Last4:     pm.Card.Last4,
			ExpMonth:  pm.Card.ExpMonth,
			ExpYear:   pm.Card.ExpYear,
			IsDefault: pm.ID == defaultPMID,
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
