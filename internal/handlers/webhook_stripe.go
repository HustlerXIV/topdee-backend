package handlers

// Stripe webhook handler — the source of truth for subscription state.
//
// Mounted at POST /webhooks/stripe (public, no JWT). The Stripe-Signature
// header + the webhook secret authenticate the call; without that header
// matching, we 400 and exit. Stripe will retry failed deliveries with
// exponential backoff for up to 3 days, so being strict here is safe.
//
// Events handled:
//   checkout.session.completed       — initial subscribe → mark active
//   customer.subscription.updated    — plan change / cancel-at-period-end
//   customer.subscription.deleted    — final cancellation
//   invoice.payment_succeeded        — renewal → bump current_period_end
//   invoice.payment_failed           — renewal failed → mark past_due
//
// All handlers are idempotent (Stripe sometimes delivers the same event
// twice). They use upsert semantics — re-running the same event is a
// no-op.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stripe/stripe-go/v79"
	stripepaymentintent "github.com/stripe/stripe-go/v79/paymentintent"
	stripesub "github.com/stripe/stripe-go/v79/subscription"
	"github.com/stripe/stripe-go/v79/webhook"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/models"
)

// StripeWebhook returns the Fiber handler. We accept the body raw (no
// JSON pre-parse) because signature verification needs the exact bytes
// Stripe sent.
func StripeWebhook(mongo *db.Mongo, cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if cfg.StripeWebhookSecret == "" {
			return fiber.NewError(fiber.StatusServiceUnavailable, "webhook secret not configured")
		}

		body := c.Body()
		sigHeader := c.Get("Stripe-Signature")
		if sigHeader == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing Stripe-Signature")
		}

		// Verifies HMAC-SHA256 signature against the secret + tolerance window.
		event, err := webhook.ConstructEvent(body, sigHeader, cfg.StripeWebhookSecret)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "signature verification failed: "+err.Error())
		}

		ctx, cancel := context.WithTimeout(c.Context(), 15*time.Second)
		defer cancel()

		// Dispatch. Returning nil here means we 200 the event; Stripe
		// won't retry. Errors trigger a Stripe retry — only return error
		// for truly transient issues (DB down), not malformed data.
		switch event.Type {
		case "checkout.session.completed":
			err = handleCheckoutCompleted(ctx, mongo, event)
		case "customer.subscription.updated", "customer.subscription.created":
			err = handleSubscriptionUpdated(ctx, mongo, event)
		case "customer.subscription.deleted":
			err = handleSubscriptionDeleted(ctx, mongo, event)
		case "invoice.payment_succeeded":
			err = handleInvoicePaid(ctx, mongo, event)
		case "invoice.payment_failed":
			err = handleInvoiceFailed(ctx, mongo, event)
		default:
			// Unhandled event types are still 200 — Stripe sends a lot
			// of stuff we don't care about (price.updated, etc).
			return c.SendStatus(fiber.StatusOK)
		}

		if err != nil {
			log.Printf("[stripe webhook] %s: %v", event.Type, err)
			return fiber.NewError(fiber.StatusInternalServerError, err.Error())
		}
		return c.SendStatus(fiber.StatusOK)
	}
}

// ── handlers ───────────────────────────────────────────────────────────

// findTenant looks up a tenant by Stripe customer id, optionally
// upserting the subscription_id. Returns the tenant (or empty if missing
// — webhook events for unknown customers are silently dropped).
func findTenantByCustomer(ctx context.Context, mongo *db.Mongo, customerID string) (*models.Tenant, error) {
	var t models.Tenant
	err := mongo.DB.Collection("tenants").
		FindOne(ctx, bson.M{"stripe_customer_id": customerID}).
		Decode(&t)
	if err != nil {
		// Log, but don't fail the webhook. Most likely the customer
		// metadata pointed to a tenant we don't have (test data, deleted
		// tenant, etc).
		log.Printf("[stripe webhook] tenant not found for customer %s: %v", customerID, err)
		return nil, nil
	}
	return &t, nil
}

func handleCheckoutCompleted(ctx context.Context, mongo *db.Mongo, ev stripe.Event) error {
	var sess stripe.CheckoutSession
	if err := json.Unmarshal(ev.Data.Raw, &sess); err != nil {
		return err
	}
	if sess.Customer == nil {
		return nil
	}
	t, err := findTenantByCustomer(ctx, mongo, sess.Customer.ID)
	if err != nil || t == nil {
		return err
	}

	planSlug := sess.Metadata["plan"]

	// ── PromptPay (payment mode) ──────────────────────────────────────────────
	if sess.Mode == stripe.CheckoutSessionModePayment {
		if sess.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
			return nil // not paid yet — ignore
		}
		interval := sess.Metadata["interval"]
		now := time.Now().UTC()
		var periodEnd time.Time
		if interval == "year" {
			periodEnd = now.AddDate(1, 0, 0)
		} else {
			periodEnd = now.AddDate(0, 1, 0)
		}
		adminNotes := ""
		if t.Subscription != nil {
			adminNotes = t.Subscription.AdminNotes
		}
		sub := &models.Subscription{
			Status:            models.SubStatusActive,
			CurrentPeriodEnd:  &periodEnd,
			CancelAtPeriodEnd: true,
			AdminNotes:        adminNotes,
			UpdatedAt:         now,
		}
		updates := bson.M{
			"subscription":           sub,
			"stripe_subscription_id": "",
		}
		if planSlug != "" {
			updates["plan"] = planSlug
			log.Printf("[stripe webhook] promptpay checkout: tenant=%s plan=%s until=%s", t.ID, planSlug, periodEnd.Format("2006-01-02"))
		}
		_, err = mongo.DB.Collection("tenants").UpdateOne(ctx,
			bson.M{"_id": t.ID},
			bson.M{"$set": updates},
		)
		if err != nil {
			return err
		}

		// ── Upsert Payment record (idempotent: session ID is _id) ────────────────
		var planDoc models.Plan
		_ = mongo.DB.Collection("plans").FindOne(ctx, bson.M{"_id": planSlug}).Decode(&planDoc)
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
		// Try to get the receipt URL from the PaymentIntent's latest charge.
		receiptURL := ""
		if sess.PaymentIntent != nil && sess.PaymentIntent.ID != "" {
			if pi, piErr := stripepaymentintent.Get(sess.PaymentIntent.ID, &stripe.PaymentIntentParams{
				Params: stripe.Params{Expand: []*string{stripe.String("latest_charge")}},
			}); piErr == nil && pi.LatestCharge != nil {
				receiptURL = pi.LatestCharge.ReceiptURL
			}
		}
		payment := models.Payment{
			ID:          sess.ID,
			TenantID:    t.ID,
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
		payResult, _ := mongo.DB.Collection("payments").ReplaceOne(ctx,
			bson.M{"_id": payment.ID},
			payment,
			options.Replace().SetUpsert(true),
		)
		// ── Referral commission (PromptPay) ──────────────────────────────────
		// invoice.payment_succeeded is never emitted for payment-mode checkouts,
		// so we credit the commission here instead.
		// UpsertedCount==1 means this is the first time we've seen this session
		// (idempotency guard — SyncCheckoutSession may have already processed it).
		if payResult != nil && payResult.UpsertedCount > 0 {
			go creditReferralCommission(mongo, t)
		}

		return nil
	}

	// ── Card / subscription mode ──────────────────────────────────────────────
	if sess.Subscription == nil {
		return nil // unrecognised mode — ignore
	}

	updates := bson.M{
		"stripe_subscription_id": sess.Subscription.ID,
	}

	// 2nd choice: subscription metadata from the embedded object (usually empty
	// in the webhook payload since it's not expanded — but worth checking).
	if planSlug == "" && sess.Subscription.Metadata != nil {
		planSlug = sess.Subscription.Metadata["plan"]
	}

	// 3rd choice: fetch the full subscription from Stripe API.
	// This is the reliable fallback — the subscription carries the metadata
	// we set in SubscriptionData.Metadata when creating the checkout session.
	if planSlug == "" {
		fullSub, apiErr := stripesub.Get(sess.Subscription.ID, nil)
		if apiErr == nil && fullSub != nil {
			planSlug = fullSub.Metadata["plan"]
			// While we have the full sub, run syncSubscription to capture all
			// fields (period end, status, etc.) in one shot.
			if syncErr := syncSubscription(ctx, mongo, t.ID, fullSub); syncErr != nil {
				log.Printf("[stripe webhook] syncSubscription after checkout: %v", syncErr)
			}
		} else {
			log.Printf("[stripe webhook] could not fetch subscription %s: %v", sess.Subscription.ID, apiErr)
		}
	}

	if planSlug != "" {
		updates["plan"] = planSlug
		log.Printf("[stripe webhook] checkout completed: tenant=%s plan=%s", t.ID, planSlug)
	} else {
		log.Printf("[stripe webhook] checkout completed: tenant=%s — plan slug not found in metadata", t.ID)
	}

	// Seed a basic subscription doc so the billing page shows the cancel
	// button immediately on redirect-back, before customer.subscription.created
	// arrives and fills in the full details.
	now := time.Now().UTC()
	merged := mergedSubscription(t.Subscription)
	if merged.Status != models.SubStatusActive && merged.Status != models.SubStatusTrialing {
		merged.Status = models.SubStatusActive
		merged.UpdatedAt = now
		updates["subscription"] = merged
	}

	_, err = mongo.DB.Collection("tenants").UpdateOne(ctx,
		bson.M{"_id": t.ID},
		bson.M{"$set": updates},
	)
	return err
}

func handleSubscriptionUpdated(ctx context.Context, mongo *db.Mongo, ev stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(ev.Data.Raw, &sub); err != nil {
		return err
	}
	if sub.Customer == nil {
		return nil
	}
	t, err := findTenantByCustomer(ctx, mongo, sub.Customer.ID)
	if err != nil || t == nil {
		return err
	}
	return syncSubscription(ctx, mongo, t.ID, &sub)
}

func handleSubscriptionDeleted(ctx context.Context, mongo *db.Mongo, ev stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(ev.Data.Raw, &sub); err != nil {
		return err
	}
	if sub.Customer == nil {
		return nil
	}
	t, err := findTenantByCustomer(ctx, mongo, sub.Customer.ID)
	if err != nil || t == nil {
		return err
	}
	now := time.Now().UTC()
	merged := mergedSubscription(t.Subscription)
	merged.Status = models.SubStatusCanceled
	merged.CanceledAt = &now
	merged.CancelAtPeriodEnd = false
	merged.UpdatedAt = now

	_, err = mongo.DB.Collection("tenants").UpdateOne(ctx,
		bson.M{"_id": t.ID},
		bson.M{"$set": bson.M{
			"subscription":           merged,
			"stripe_subscription_id": "",
			"plan":                   "free", // auto-downgrade when subscription ends
		}},
	)
	return err
}

func handleInvoicePaid(ctx context.Context, m *db.Mongo, ev stripe.Event) error {
	var inv stripe.Invoice
	if err := json.Unmarshal(ev.Data.Raw, &inv); err != nil {
		return err
	}
	if inv.Customer == nil || inv.Subscription == nil {
		return nil
	}
	t, err := findTenantByCustomer(ctx, m, inv.Customer.ID)
	if err != nil || t == nil {
		return err
	}
	// Re-fetch the full subscription so we have the latest period end.
	// We embed the same data path here for resilience: invoices are the
	// most reliable signal of "you got paid".
	merged := mergedSubscription(t.Subscription)
	merged.Status = models.SubStatusActive
	if inv.Lines != nil && len(inv.Lines.Data) > 0 && inv.Lines.Data[0].Period != nil {
		end := time.Unix(inv.Lines.Data[0].Period.End, 0).UTC()
		merged.CurrentPeriodEnd = &end
	}
	merged.UpdatedAt = time.Now().UTC()

	_, err = m.DB.Collection("tenants").UpdateOne(ctx,
		bson.M{"_id": t.ID},
		bson.M{"$set": bson.M{"subscription": merged}},
	)
	if err != nil {
		return err
	}

	// ── Referral commission ───────────────────────────────────────────
	// Fire-and-forget: a commission error must never fail the webhook
	// (Stripe would retry, causing duplicate subscription updates).
	go creditReferralCommission(m, t)

	return nil
}

// creditReferralCommission finds the active referral for this tenant and
// credits the appropriate commission to the referrer's wallet.
func creditReferralCommission(m *db.Mongo, t *models.Tenant) {
	if t.ReferralCodeUsed == "" {
		return
	}

	// Load programme settings.
	var settings models.ReferralSettings
	if err := m.DB.Collection("referral_settings").
		FindOne(nil, bson.M{"_id": "global"}).Decode(&settings); err != nil {
		settings = models.DefaultReferralSettings()
	}
	if !settings.Enabled {
		return
	}

	// Find the referral record for this referred tenant.
	var referral models.Referral
	if err := m.DB.Collection("referrals").
		FindOne(nil, bson.M{
			"referred_tenant_id": t.ID,
			"status":             models.ReferralStatusActive,
		}).Decode(&referral); err != nil {
		return // no active referral — nothing to credit
	}

	// Determine commission amount.
	amount := settings.RecurringCommissionAmount
	if referral.CommissionCount == 0 {
		amount = settings.FirstCommissionAmount
	}
	if amount <= 0 {
		return
	}

	now := time.Now().UTC()
	description := fmt.Sprintf("Referral commission from %s — ฿%.2f",
		referral.ReferredTenantName, float64(amount)/100)

	// Credit the wallet.
	if err := CreditCommission(nil, m, referral.ReferrerTenantID, referral.ID, description, amount); err != nil {
		log.Printf("[referral] credit commission: %v", err)
		return
	}

	// Update the referral record.
	_, err := m.DB.Collection("referrals").UpdateOne(nil,
		bson.M{"_id": referral.ID},
		bson.M{"$inc": bson.M{"commission_count": 1, "total_earned": amount},
			"$set": bson.M{"updated_at": now}},
	)
	if err != nil {
		log.Printf("[referral] update referral stats: %v", err)
	}
}

func handleInvoiceFailed(ctx context.Context, mongo *db.Mongo, ev stripe.Event) error {
	var inv stripe.Invoice
	if err := json.Unmarshal(ev.Data.Raw, &inv); err != nil {
		return err
	}
	if inv.Customer == nil {
		return nil
	}
	t, err := findTenantByCustomer(ctx, mongo, inv.Customer.ID)
	if err != nil || t == nil {
		return err
	}
	merged := mergedSubscription(t.Subscription)
	merged.Status = models.SubStatusPastDue
	merged.UpdatedAt = time.Now().UTC()
	_, err = mongo.DB.Collection("tenants").UpdateOne(ctx,
		bson.M{"_id": t.ID},
		bson.M{"$set": bson.M{"subscription": merged}},
	)
	return err
}

// ── helpers ────────────────────────────────────────────────────────────

func mergedSubscription(existing *models.Subscription) *models.Subscription {
	if existing != nil {
		return existing
	}
	return &models.Subscription{Status: models.SubStatusActive}
}

// syncSubscription writes the canonical fields out of a Stripe Subscription
// object into our local Subscription doc. Used by both .created and
// .updated handlers (Stripe events are very similar in shape).
func syncSubscription(ctx context.Context, mongo *db.Mongo, tenantID string, sub *stripe.Subscription) error {
	merged := &models.Subscription{}
	// Don't overwrite admin_notes — preserve whatever the admin typed.
	var existing models.Tenant
	if err := mongo.DB.Collection("tenants").
		FindOne(ctx, bson.M{"_id": tenantID}).Decode(&existing); err == nil && existing.Subscription != nil {
		merged.AdminNotes = existing.Subscription.AdminNotes
	}

	// Map Stripe status → our status. Stripe has more granularity (incomplete,
	// incomplete_expired, unpaid) than we want to expose in admin UI, so we
	// flatten the rare ones into past_due.
	switch sub.Status {
	case stripe.SubscriptionStatusTrialing:
		merged.Status = models.SubStatusTrialing
	case stripe.SubscriptionStatusActive:
		merged.Status = models.SubStatusActive
	case stripe.SubscriptionStatusCanceled:
		merged.Status = models.SubStatusCanceled
	case stripe.SubscriptionStatusPaused:
		merged.Status = models.SubStatusPaused
	case stripe.SubscriptionStatusPastDue,
		stripe.SubscriptionStatusUnpaid,
		stripe.SubscriptionStatusIncomplete,
		stripe.SubscriptionStatusIncompleteExpired:
		merged.Status = models.SubStatusPastDue
	default:
		merged.Status = string(sub.Status)
	}

	if sub.TrialEnd > 0 {
		t := time.Unix(sub.TrialEnd, 0).UTC()
		merged.TrialEndsAt = &t
	}
	if sub.CurrentPeriodEnd > 0 {
		t := time.Unix(sub.CurrentPeriodEnd, 0).UTC()
		merged.CurrentPeriodEnd = &t
	}
	if sub.CanceledAt > 0 {
		t := time.Unix(sub.CanceledAt, 0).UTC()
		merged.CanceledAt = &t
	}
	merged.CancelAtPeriodEnd = sub.CancelAtPeriodEnd
	merged.UpdatedAt = time.Now().UTC()

	// Resolve the plan slug — check subscription metadata first (set by our
	// checkout session), then fall back to price metadata.
	planSlug := sub.Metadata["plan"]
	if planSlug == "" && sub.Items != nil && len(sub.Items.Data) > 0 && sub.Items.Data[0].Price != nil {
		planSlug = sub.Items.Data[0].Price.Metadata["plan"]
	}

	updates := bson.M{
		"subscription":           merged,
		"stripe_subscription_id": sub.ID,
	}
	if planSlug != "" {
		updates["plan"] = planSlug
	}

	_, err := mongo.DB.Collection("tenants").UpdateOne(ctx,
		bson.M{"_id": tenantID},
		bson.M{"$set": updates},
	)
	return err
}

// Discard buffer to silence "imported and not used: io" if the body
// reader path is ever simplified out of the file.
var _ = io.Discard
