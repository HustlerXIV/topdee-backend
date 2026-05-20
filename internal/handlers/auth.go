package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/bcrypt"

	"github.com/topdee/backend/internal/auth"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/email"
	"github.com/topdee/backend/internal/models"
)

type AuthHandler struct {
	mongo *db.Mongo
	cfg   *config.Config
}

func NewAuthHandler(m *db.Mongo, c *config.Config) *AuthHandler {
	return &AuthHandler{mongo: m, cfg: c}
}

type registerReq struct {
	TenantName string `json:"tenant_name"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	// AcceptedPrivacy must be true for registration to succeed.
	// The timestamp is stored on the user record for legal compliance.
	AcceptedPrivacy bool `json:"accepted_privacy"`
	// ReferralCode is the optional word-of-mouth code entered at signup.
	// If valid, the new tenant gets a discount and the referrer earns commission.
	ReferralCode string `json:"referral_code"`
}

// isBootstrapAdmin returns true if the email is in BOOTSTRAP_ADMIN_EMAILS.
// Used to auto-promote the first admin(s) on register without needing a
// pre-existing admin to do it.
func (h *AuthHandler) isBootstrapAdmin(email string) bool {
	for _, e := range h.cfg.BootstrapAdminEmails {
		if e == email {
			return true
		}
	}
	return false
}

// Register creates a new tenant and an owner user.
func (h *AuthHandler) Register(c *fiber.Ctx) error {
	var req registerReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.TenantName == "" || req.Email == "" || len(req.Password) < 8 {
		return fiber.NewError(fiber.StatusBadRequest, "tenant_name, email, password (>=8) required")
	}
	if !req.AcceptedPrivacy {
		return fiber.NewError(fiber.StatusBadRequest, "you must accept the Privacy Policy to register")
	}

	now := time.Now().UTC()

	// Look up the free plan's expiry_days so we can auto-stamp a trial end
	// date. Best-effort — if the plans collection isn't seeded yet we just
	// leave subscription nil and the tenant has unlimited access until an
	// admin sets it manually.
	var initialSubscription *models.Subscription
	var freePlan struct {
		ExpiryDays int `bson:"expiry_days"`
	}
	if err := h.mongo.DB.Collection("plans").
		FindOne(c.Context(), bson.M{"_id": "free"}).Decode(&freePlan); err == nil {
		if freePlan.ExpiryDays > 0 {
			trialEnd := now.AddDate(0, 0, freePlan.ExpiryDays)
			initialSubscription = &models.Subscription{
				Status:      models.SubStatusTrialing,
				TrialEndsAt: &trialEnd,
				UpdatedAt:   now,
			}
		}
	}

	tenant := models.Tenant{
		ID:           uuid.NewString(),
		Name:         req.TenantName,
		Plan:         "free",
		Subscription: initialSubscription,
		CreatedAt:    now,
	}
	if _, err := h.mongo.DB.Collection("tenants").InsertOne(c.Context(), tenant); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user := models.User{
		ID:                uuid.NewString(),
		TenantID:          tenant.ID,
		Email:             req.Email,
		PasswordHash:      string(hash),
		Role:              "owner",
		IsPlatformAdmin:   h.isBootstrapAdmin(req.Email),
		PrivacyAcceptedAt: &now,
		CreatedAt:         now,
	}
	if _, err := h.mongo.DB.Collection("users").InsertOne(c.Context(), user); err != nil {
		// duplicate email
		if mongo.IsDuplicateKeyError(err) {
			return fiber.NewError(fiber.StatusConflict, "email already registered")
		}
		return err
	}

	// ── Referral code handling ────────────────────────────────────────
	// Best-effort: a referral error never blocks signup, just logs.
	go h.processReferralSignup(tenant.ID, user.ID, strings.TrimSpace(strings.ToUpper(req.ReferralCode)), tenant.Name, now)

	// ── Auto-generate this new tenant's own referral code ─────────────
	go h.autoGenerateReferralCode(tenant.ID, user.ID, tenant.Name)

	token, err := auth.IssueToken(auth.IssueOpts{
		Secret: h.cfg.JWTSecret, UserID: user.ID, TenantID: tenant.ID,
		Email: user.Email, Role: user.Role, IsAdmin: user.IsPlatformAdmin,
		TTLHours: h.cfg.JWTTTLHours,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"token":  token,
		"user":   user,
		"tenant": tenant,
	})
}

// processReferralSignup links the new tenant to the code's referrer,
// records the Referral document, and stamps the discount on the tenant.
// Runs in a goroutine — never blocks the register response.
func (h *AuthHandler) processReferralSignup(newTenantID, newUserID, code, tenantName string, now time.Time) {
	if code == "" {
		return
	}

	ctx := context.Background()

	// Load the programme settings to get discount config.
	var settings models.ReferralSettings
	err := h.mongo.DB.Collection("referral_settings").
		FindOne(ctx, bson.M{"_id": "global"}).Decode(&settings)
	if err == mongo.ErrNoDocuments {
		settings = models.DefaultReferralSettings()
	} else if err != nil {
		log.Printf("[referral] load settings: %v", err)
		return
	}
	if !settings.Enabled {
		return
	}

	// Validate the code.
	var refCode models.ReferralCode
	if err := h.mongo.DB.Collection("referral_codes").
		FindOne(ctx, bson.M{"_id": code}).Decode(&refCode); err != nil {
		// Invalid code — silently ignore.
		return
	}
	// Prevent self-referral.
	if refCode.TenantID == newTenantID {
		return
	}

	// Stamp discount on the new tenant.
	// For "first_purchase" the expiry is set far in the future — the webhook
	// clears it as soon as the first payment lands, so the date is just a
	// safety-net fallback. For "duration" the expiry controls how long the
	// discount is valid across renewals.
	discountType := settings.DiscountType
	if discountType == "" {
		discountType = models.DiscountTypeFirstPurchase
	}
	var discountExpiry time.Time
	if discountType == models.DiscountTypeDuration {
		discountExpiry = now.AddDate(0, settings.DiscountDurationMonths, 0)
	} else {
		discountExpiry = now.AddDate(10, 0, 0) // first_purchase: cleared by webhook, far-future fallback
	}
	_, err = h.mongo.DB.Collection("tenants").UpdateOne(
		ctx,
		bson.M{"_id": newTenantID},
		bson.M{"$set": bson.M{
			"referral_code_used":           code,
			"referral_discount_expires_at": discountExpiry,
			"referral_discount_type":       discountType,
		}},
	)
	if err != nil {
		log.Printf("[referral] stamp discount: %v", err)
		return
	}

	// Create the referral record.
	referral := models.Referral{
		ID:                 uuid.NewString(),
		Code:               code,
		ReferrerTenantID:   refCode.TenantID,
		ReferrerUserID:     refCode.UserID,
		ReferredTenantID:   newTenantID,
		ReferredTenantName: tenantName,
		Status:             models.ReferralStatusActive,
		CommissionCount:    0,
		TotalEarned:        0,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if _, err := h.mongo.DB.Collection("referrals").InsertOne(ctx, referral); err != nil {
		log.Printf("[referral] insert referral: %v", err)
	}
}

// autoGenerateReferralCode creates a referral code for a new tenant owner.
// Runs in a goroutine — non-blocking.
func (h *AuthHandler) autoGenerateReferralCode(tenantID, userID, tenantName string) {
	ctx := context.Background()
	// Check if one already exists (e.g. Google OAuth signup may call Register path).
	count, _ := h.mongo.DB.Collection("referral_codes").
		CountDocuments(ctx, bson.M{"tenant_id": tenantID})
	if count > 0 {
		return
	}

	codeStr, err := generateReferralCode(h.mongo, tenantName)
	if err != nil {
		log.Printf("[referral] auto-generate code: %v", err)
		return
	}
	code := models.ReferralCode{
		ID:        codeStr,
		TenantID:  tenantID,
		UserID:    userID,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := h.mongo.DB.Collection("referral_codes").InsertOne(ctx, code); err != nil {
		if !mongo.IsDuplicateKeyError(err) {
			log.Printf("[referral] insert auto-code: %v", err)
		}
	}
}

// generateReferralCode produces a unique, human-friendly code like "NAPAT24".
func generateReferralCode(m *db.Mongo, tenantName string) (string, error) {
	var letters []rune
	for _, r := range tenantName {
		if r >= 'A' && r <= 'Z' {
			letters = append(letters, r)
		} else if r >= 'a' && r <= 'z' {
			letters = append(letters, r-32)
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

	ctx := context.Background()
	for i := 0; i < 200; i++ {
		code := candidate
		if i > 0 {
			code = fmt.Sprintf("%s%d", candidate, i)
		}
		n, _ := m.DB.Collection("referral_codes").CountDocuments(ctx, bson.M{"_id": code})
		if n == 0 {
			return code, nil
		}
	}
	return base + uuid.NewString()[:4], nil
}

// dummy is a placeholder to keep the options import used.
func (h *AuthHandler) dummy() { _ = options.Update() }

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// ── Forgot / Reset password ───────────────────────────────────────────────────

// ForgotPassword accepts an email address, generates a time-limited reset
// token, stores its SHA-256 hash on the user, and sends a reset link via
// Resend. Returns 404 when the email is not registered so the user knows
// they need to sign up first.
func (h *AuthHandler) ForgotPassword(c *fiber.Ctx) error {
	var req struct {
		Email string `json:"email"`
	}
	if err := c.BodyParser(&req); err != nil || req.Email == "" {
		return fiber.NewError(fiber.StatusBadRequest, "email required")
	}

	// Look up the user — tell them explicitly if not found.
	var u models.User
	err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"email": req.Email}).Decode(&u)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "no account found with that email — please register first")
	}

	// Generate 32 random bytes → hex token (64 chars).
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("forgot-password: rand: %w", err)
	}
	plainToken := hex.EncodeToString(raw)

	// Hash the token for storage — we never store the plaintext.
	sum := sha256.Sum256([]byte(plainToken))
	tokenHash := hex.EncodeToString(sum[:])
	expiresAt := time.Now().UTC().Add(1 * time.Hour)

	_, err = h.mongo.DB.Collection("users").UpdateOne(
		c.Context(),
		bson.M{"_id": u.ID},
		bson.M{"$set": bson.M{
			"password_reset_token_hash":  tokenHash,
			"password_reset_expires_at": expiresAt,
		}},
	)
	if err != nil {
		return err
	}

	// Build the reset URL and fire the email in the background.
	resetURL := h.cfg.FrontendBaseURL + "/reset-password?token=" + plainToken
	mailer := &email.Mailer{APIKey: h.cfg.ResendAPIKey, From: h.cfg.EmailFrom}
	go func() {
		_ = mailer.Send(
			u.Email,
			"Reset your Topdee password",
			email.ForgotPasswordHTML(u.Name, resetURL),
		)
	}()

	return c.JSON(fiber.Map{"ok": true})
}

// ResetPassword validates the reset token and updates the user's password.
// The token is the raw (unhashed) value from the email link.
func (h *AuthHandler) ResetPassword(c *fiber.Ctx) error {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.Token == "" || len(req.Password) < 8 {
		return fiber.NewError(fiber.StatusBadRequest, "token and password (>=8 chars) required")
	}

	// Hash the incoming token to compare against the stored hash.
	sum := sha256.Sum256([]byte(req.Token))
	tokenHash := hex.EncodeToString(sum[:])

	var u models.User
	err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"password_reset_token_hash": tokenHash}).Decode(&u)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid or expired token")
	}

	// Check expiry.
	if u.PasswordResetExpiresAt == nil || time.Now().UTC().After(*u.PasswordResetExpiresAt) {
		return fiber.NewError(fiber.StatusBadRequest, "reset link has expired — please request a new one")
	}

	// Hash the new password.
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	// Save new password and clear the reset token atomically.
	_, err = h.mongo.DB.Collection("users").UpdateOne(
		c.Context(),
		bson.M{"_id": u.ID},
		bson.M{
			"$set":   bson.M{"password_hash": string(hash)},
			"$unset": bson.M{"password_reset_token_hash": "", "password_reset_expires_at": ""},
		},
	)
	if err != nil {
		return err
	}

	return c.JSON(fiber.Map{"ok": true})
}

func (h *AuthHandler) Login(c *fiber.Ctx) error {
	var req loginReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	var u models.User
	err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"email": req.Email}).Decode(&u)
	if err != nil {
		return fiber.NewError(fiber.StatusUnauthorized, "invalid credentials")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)); err != nil {
		return fiber.NewError(fiber.StatusUnauthorized, "invalid credentials")
	}
	if u.Suspended {
		return fiber.NewError(fiber.StatusForbidden, "account suspended — contact support")
	}
	token, err := auth.IssueToken(auth.IssueOpts{
		Secret: h.cfg.JWTSecret, UserID: u.ID, TenantID: u.TenantID,
		Email: u.Email, Role: u.Role, IsAdmin: u.IsPlatformAdmin,
		TTLHours: h.cfg.JWTTTLHours,
	})
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"token": token,
		"user":  u,
	})
}
