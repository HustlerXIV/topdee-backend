package handlers

// Stripe billing — tenant-facing self-service.
//
// Two endpoints, mirroring the two flows the user sees on /billing:
//
//   POST /api/v1/billing/checkout-session  — first-time subscribe / plan upgrade
//   POST /api/v1/billing/portal-session    — manage existing sub (change card,
//                                            upgrade, cancel, see invoices)
//
// Stripe Customer is created lazily — we don't make one until a tenant
// actually hits Checkout, so workspaces that never pay don't pollute
// Stripe with empty customers.
//
// The webhook (in webhook_stripe.go) is what flips our local Subscription
// state. These endpoints only kick off Stripe-hosted flows; we never
// trust the response/redirect for state changes.

import (
	"github.com/gofiber/fiber/v2"
	"github.com/stripe/stripe-go/v79"
	billingportal "github.com/stripe/stripe-go/v79/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v79/checkout/session"
	stripecustomer "github.com/stripe/stripe-go/v79/customer"
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

type checkoutReq struct {
	Plan string `json:"plan"` // "starter" | "growth" | "pro" | "enterprise"
}

// POST /api/v1/billing/checkout-session
//
// Returns { url } — frontend redirects the user there. Stripe Checkout
// handles card entry, 3DS, PromptPay (where supported), receipts, etc.
// On success Stripe redirects back to BILLING_RETURN_URL?success=1 and
// fires the checkout.session.completed webhook in parallel.
func (h *BillingHandler) CreateCheckoutSession(c *fiber.Ctx) error {
	if h.cfg.StripeSecretKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "billing not configured")
	}
	tid := middleware.TenantID(c)

	var req checkoutReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	priceID := h.cfg.StripePrices[req.Plan]
	if priceID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "unknown or unconfigured plan")
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
		Mode:     stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer: stripe.String(customerID),
		LineItems: []*stripe.CheckoutSessionLineItemParams{{
			Price:    stripe.String(priceID),
			Quantity: stripe.Int64(1),
		}},
		// Prevent customer from accidentally creating a duplicate sub if
		// they already have one — Stripe will redirect to portal instead.
		AllowPromotionCodes: stripe.Bool(true),
		ClientReferenceID:   stripe.String(t.ID),
		SuccessURL:          stripe.String(h.cfg.BillingReturnURL + "?status=success&session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:           stripe.String(h.cfg.BillingReturnURL + "?status=cancel"),
		// Metadata travels through to the webhook so we can attribute
		// the resulting subscription back to our tenant without a lookup.
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{
			Metadata: map[string]string{"tenant_id": t.ID, "plan": req.Plan},
		},
	}
	sess, err := checkoutsession.New(params)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "stripe checkout: "+err.Error())
	}
	return c.JSON(fiber.Map{"url": sess.URL})
}

// POST /api/v1/billing/portal-session
//
// Returns { url } for the Stripe Customer Portal. The portal lets the
// user change card, upgrade/downgrade plan, view invoices, cancel — all
// hosted by Stripe. Updates flow back via webhooks.
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
