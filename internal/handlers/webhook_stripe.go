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
	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v79"
	stripecharge "github.com/stripe/stripe-go/v79/charge"
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

		// ── Idempotency claim ────────────────────────────────────────────
		// Stripe delivers events at-least-once (redelivery on slow ack,
		// retries, etc.). Several handlers here have side effects that are
		// NOT naturally idempotent — most importantly crediting referral
		// commission with an unconditional $inc. We claim each event id in
		// `stripe_events`; a duplicate delivery finds it already claimed and
		// is skipped. If the handler later fails, we release the claim so
		// Stripe's retry can reprocess it.
		claim, cerr := mongo.DB.Collection("stripe_events").UpdateOne(ctx,
			bson.M{"_id": event.ID},
			bson.M{"$setOnInsert": bson.M{"type": string(event.Type), "created_at": time.Now().UTC()}},
			options.Update().SetUpsert(true),
		)
		if cerr != nil {
			// Can't guarantee dedupe → fail so Stripe retries rather than
			// risk double-processing.
			return fiber.NewError(fiber.StatusInternalServerError, "dedupe claim: "+cerr.Error())
		}
		if claim.UpsertedCount == 0 {
			log.Printf("[stripe webhook] duplicate event %s (%s), skipping", event.ID, event.Type)
			return c.SendStatus(fiber.StatusOK)
		}

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
		case "charge.refunded":
			err = handleChargeRefunded(ctx, mongo, event)
		case "charge.dispute.created":
			err = handleChargeDisputeCreated(ctx, mongo, event)
		case "checkout.session.async_payment_failed":
			err = handleAsyncPaymentFailed(ctx, mongo, event)
		default:
			// Unhandled event types are still 200 — Stripe sends a lot
			// of stuff we don't care about (price.updated, etc).
			return c.SendStatus(fiber.StatusOK)
		}

		if err != nil {
			// Release the idempotency claim so Stripe's retry reprocesses this
			// event; otherwise a transient failure would drop it permanently.
			_, _ = mongo.DB.Collection("stripe_events").DeleteOne(ctx, bson.M{"_id": event.ID})
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
		tenantUpdate := bson.M{"$set": updates}
		if t.ReferralDiscountType == models.DiscountTypeFirstPurchase || t.ReferralDiscountType == "" {
			tenantUpdate["$unset"] = bson.M{"referral_discount_expires_at": ""}
		}
		_, err = mongo.DB.Collection("tenants").UpdateOne(ctx,
			bson.M{"_id": t.ID},
			tenantUpdate,
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
			// Resume a paused referral before crediting (see handleInvoicePaid).
			setReferralStatusBestEffort(ctx, mongo, t.ID, models.ReferralStatusPaused, models.ReferralStatusActive, "promptpay paid")
			go creditReferralCommission(mongo, t)
		}

		return nil
	}

	// ── Card / subscription mode ──────────────────────────────────────────────
	if sess.Subscription == nil {
		return nil // unrecognised mode — ignore
	}

	// Guard: don't seed the plan as active when the checkout is explicitly
	// unpaid. For subscription mode Stripe normally sets payment_status="paid"
	// (or leaves it empty in some payloads), so we only bail on an explicit
	// "unpaid" — the subscription lifecycle webhooks will grant access once the
	// first invoice is actually collected.
	if sess.PaymentStatus == stripe.CheckoutSessionPaymentStatusUnpaid {
		return nil
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
	// Pause the referral so no further commissions accrue while this tenant is
	// canceled; it resumes on their next successful payment.
	setReferralStatusBestEffort(ctx, mongo, t.ID, models.ReferralStatusActive, models.ReferralStatusPaused, "subscription canceled")
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

	invoiceUpdate := bson.M{"$set": bson.M{"subscription": merged}}
	if t.ReferralDiscountType == models.DiscountTypeFirstPurchase || t.ReferralDiscountType == "" {
		invoiceUpdate["$unset"] = bson.M{"referral_discount_expires_at": ""}
	}
	_, err = m.DB.Collection("tenants").UpdateOne(ctx,
		bson.M{"_id": t.ID},
		invoiceUpdate,
	)
	if err != nil {
		return err
	}

	// ── Referral commission ───────────────────────────────────────────
	// Only pay commission on invoices that actually collected money. Stripe
	// emits invoice.payment_succeeded for ฿0 invoices too (trial-start
	// invoices, 100%-off coupons) — those must not generate real commission.
	// Fire-and-forget: a commission error must never fail the webhook
	// (Stripe would retry, causing duplicate subscription updates).
	if inv.AmountPaid > 0 {
		// Resume a paused referral BEFORE crediting: creditReferralCommission
		// only matches status=active, so the flip must land first. This does not
		// double-credit — crediting is separate and guarded by AmountPaid>0 and
		// the event-ID idempotency in the dispatcher.
		setReferralStatusBestEffort(ctx, m, t.ID, models.ReferralStatusPaused, models.ReferralStatusActive, "invoice paid")
		go creditReferralCommission(m, t)
	}

	return nil
}

// creditReferralCommission finds the active referral for this tenant and
// credits the appropriate commission to the referrer's wallet.
// Called as a goroutine — must create its own context since the request
// context is already cancelled by the time we get here.
func creditReferralCommission(m *db.Mongo, t *models.Tenant) {
	if t.ReferralCodeUsed == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Load programme settings.
	var settings models.ReferralSettings
	if err := m.DB.Collection("referral_settings").
		FindOne(ctx, bson.M{"_id": "global"}).Decode(&settings); err != nil {
		settings = models.DefaultReferralSettings()
	}
	if !settings.Enabled {
		return
	}

	// Find the referral record for this referred tenant.
	var referral models.Referral
	if err := m.DB.Collection("referrals").
		FindOne(ctx, bson.M{
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
	if err := CreditCommission(ctx, m, referral.ReferrerTenantID, referral.ID, description, amount); err != nil {
		log.Printf("[referral] credit commission: %v", err)
		return
	}

	// Update the referral record.
	_, err := m.DB.Collection("referrals").UpdateOne(ctx,
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
	// Pause the referral while the account is past_due (no commission on money
	// we didn't collect). It resumes on the next successful payment. The plan
	// itself is NOT downgraded here — dunning may recover; ExpireSubscriptions
	// handles the downgrade once the grace period lapses.
	setReferralStatusBestEffort(ctx, mongo, t.ID, models.ReferralStatusActive, models.ReferralStatusPaused, "invoice payment failed")
	return err
}

// handleChargeRefunded revokes access when a charge is fully refunded and
// claws back any referral commission that was paid on it. Partial refunds are
// ignored (the customer keeps what they partly paid for). This is the main
// protection for PromptPay one-time payments, where a refund would otherwise
// leave the tenant on the paid plan for the full period.
func handleChargeRefunded(ctx context.Context, mongo *db.Mongo, ev stripe.Event) error {
	var ch stripe.Charge
	if err := json.Unmarshal(ev.Data.Raw, &ch); err != nil {
		return err
	}
	// Only act on a full refund; a partial refund keeps the plan.
	if !ch.Refunded && ch.AmountRefunded < ch.Amount {
		return nil
	}
	if ch.Customer == nil {
		return nil
	}
	t, err := findTenantByCustomer(ctx, mongo, ch.Customer.ID)
	if err != nil || t == nil {
		return err
	}
	return revokeAccessAndReverseCommission(ctx, mongo, t, "charge refunded")
}

// handleChargeDisputeCreated treats a new dispute (chargeback) like a refund:
// the customer is challenging the payment, so access is revoked and commission
// is reversed. The dispute's charge often doesn't carry the customer inline in
// the webhook payload, so we fetch the charge when needed.
func handleChargeDisputeCreated(ctx context.Context, mongo *db.Mongo, ev stripe.Event) error {
	var dp stripe.Dispute
	if err := json.Unmarshal(ev.Data.Raw, &dp); err != nil {
		return err
	}
	var customerID string
	if dp.Charge != nil {
		if dp.Charge.Customer != nil {
			customerID = dp.Charge.Customer.ID
		} else if dp.Charge.ID != "" {
			if ch, cerr := stripecharge.Get(dp.Charge.ID, nil); cerr == nil && ch.Customer != nil {
				customerID = ch.Customer.ID
			}
		}
	}
	if customerID == "" {
		return nil
	}
	t, err := findTenantByCustomer(ctx, mongo, customerID)
	if err != nil || t == nil {
		return err
	}
	return revokeAccessAndReverseCommission(ctx, mongo, t, "payment disputed")
}

// handleAsyncPaymentFailed fires when a delayed payment method (e.g. PromptPay)
// ultimately fails after checkout. We mark the payment record failed. We do NOT
// downgrade the plan here: the completed-handler only grants a PromptPay plan
// once payment_status=paid, so a failed async payment was never granted — and
// blindly downgrading could wrongly cut off a tenant who still has a separate
// active plan.
func handleAsyncPaymentFailed(ctx context.Context, mongo *db.Mongo, ev stripe.Event) error {
	var sess stripe.CheckoutSession
	if err := json.Unmarshal(ev.Data.Raw, &sess); err != nil {
		return err
	}
	if sess.ID == "" {
		return nil
	}
	_, _ = mongo.DB.Collection("payments").UpdateOne(ctx,
		bson.M{"_id": sess.ID},
		bson.M{"$set": bson.M{"status": "failed"}},
	)
	log.Printf("[stripe webhook] async payment failed for session=%s", sess.ID)
	return nil
}

// revokeAccessAndReverseCommission is shared by the refund + dispute handlers.
// It downgrades one-time (PromptPay) grants to free, reverses any referral
// commission that was paid, and marks the most recent paid payment refunded.
//
// Card subscribers (stripe_subscription_id set) are intentionally NOT force-
// downgraded here — their access is governed by the subscription lifecycle
// (customer.subscription.updated/deleted); a single refunded/disputed invoice
// shouldn't necessarily sever an otherwise-active recurring subscription.
func revokeAccessAndReverseCommission(ctx context.Context, mongo *db.Mongo, t *models.Tenant, reason string) error {
	now := time.Now().UTC()

	if t.StripeSubscriptionID == "" {
		merged := mergedSubscription(t.Subscription)
		merged.Status = models.SubStatusCanceled
		merged.CanceledAt = &now
		merged.CancelAtPeriodEnd = false
		merged.UpdatedAt = now
		if _, err := mongo.DB.Collection("tenants").UpdateOne(ctx,
			bson.M{"_id": t.ID},
			bson.M{"$set": bson.M{
				"subscription": merged,
				"plan":         "free",
			}},
		); err != nil {
			return err
		}
		log.Printf("[stripe webhook] revoked access for tenant=%s (%s)", t.ID, reason)
	}

	// Mark the tenant's most recent paid payment as refunded (best-effort).
	_ = mongo.DB.Collection("payments").FindOneAndUpdate(ctx,
		bson.M{"tenant_id": t.ID, "status": "paid"},
		bson.M{"$set": bson.M{"status": "refunded"}},
		options.FindOneAndUpdate().SetSort(bson.D{{Key: "created_at", Value: -1}}),
	).Err()

	// Claw back the referral commission that was paid on this revenue.
	reverseReferralCommission(mongo, t, reason)
	return nil
}

// reverseReferralCommission reverses the most recent commission credited for a
// referred tenant when its payment is refunded/disputed. The wallet is clamped
// at zero (the referrer may have already withdrawn), and referral stats are
// rolled back. Best-effort: failures are logged, never fatal to the webhook.
func reverseReferralCommission(m *db.Mongo, t *models.Tenant, reason string) {
	if t.ReferralCodeUsed == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var referral models.Referral
	if err := m.DB.Collection("referrals").
		FindOne(ctx, bson.M{"referred_tenant_id": t.ID}).Decode(&referral); err != nil {
		return
	}

	// Most recent commission credit for this referral (positive amount).
	var lastCredit models.WalletTransaction
	if err := m.DB.Collection("wallet_transactions").FindOne(ctx,
		bson.M{"referral_id": referral.ID, "type": models.TxnTypeCommission, "amount": bson.M{"$gt": 0}},
		options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}}),
	).Decode(&lastCredit); err != nil {
		return // nothing to reverse
	}
	amount := lastCredit.Amount
	if amount <= 0 {
		return
	}

	now := time.Now().UTC()

	// Clamp the deduction so the wallet never goes negative.
	var wallet models.Wallet
	_ = m.DB.Collection("wallets").FindOne(ctx, bson.M{"_id": referral.ReferrerTenantID}).Decode(&wallet)
	deduct := amount
	if wallet.Balance < deduct {
		deduct = wallet.Balance
	}
	if deduct > 0 {
		_, _ = m.DB.Collection("wallets").UpdateOne(ctx,
			bson.M{"_id": referral.ReferrerTenantID},
			bson.M{"$inc": bson.M{"balance": -deduct}, "$set": bson.M{"updated_at": now}},
		)
	}

	txn := models.WalletTransaction{
		ID:          uuid.NewString(),
		TenantID:    referral.ReferrerTenantID,
		Type:        models.TxnTypeCommissionReversal,
		Amount:      -amount,
		ReferralID:  referral.ID,
		Description: fmt.Sprintf("Commission reversed (%s) — %s", reason, referral.ReferredTenantName),
		CreatedAt:   now,
	}
	if _, err := m.DB.Collection("wallet_transactions").InsertOne(ctx, txn); err != nil {
		log.Printf("[referral] reversal txn: %v", err)
	}

	_, _ = m.DB.Collection("referrals").UpdateOne(ctx,
		bson.M{"_id": referral.ID},
		bson.M{
			"$inc": bson.M{"commission_count": -1, "total_earned": -amount},
			"$set": bson.M{"updated_at": now},
		},
	)
}

// setReferralStatusBestEffort transitions a referred tenant's referral record
// from one status to another — active→paused when they lapse (cancel/past_due)
// so no further commissions accrue, and paused→active when they pay again so
// future commissions resume. It only flips the gating status; it never
// credits/debits money (that is handled separately and already guarded against
// double-crediting). Best-effort: errors are logged, never fatal to the webhook.
func setReferralStatusBestEffort(ctx context.Context, mongo *db.Mongo, tenantID, from, to, reason string) {
	res, err := mongo.DB.Collection("referrals").UpdateOne(ctx,
		bson.M{"referred_tenant_id": tenantID, "status": from},
		bson.M{"$set": bson.M{"status": to, "updated_at": time.Now().UTC()}},
	)
	if err != nil {
		log.Printf("[referral] status %s→%s tenant=%s (%s): %v", from, to, tenantID, reason, err)
		return
	}
	if res.ModifiedCount > 0 {
		log.Printf("[referral] status %s→%s tenant=%s (%s)", from, to, tenantID, reason)
	}
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

	// Resolve the plan slug. On a Customer Portal plan change Stripe does NOT
	// update the subscription's own metadata["plan"] — it keeps whatever we set
	// at the original checkout, so it can go stale. The line item's Price
	// metadata always reflects the CURRENT price, so prefer a non-empty price
	// metadata plan and only fall back to subscription metadata when the price
	// carries no slug. This keeps portal upgrades/downgrades in sync.
	planSlug := ""
	if sub.Items != nil && len(sub.Items.Data) > 0 && sub.Items.Data[0].Price != nil {
		planSlug = sub.Items.Data[0].Price.Metadata["plan"]
	}
	if planSlug == "" {
		planSlug = sub.Metadata["plan"]
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
