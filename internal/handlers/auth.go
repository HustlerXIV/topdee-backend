package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/crypto/bcrypt"

	"github.com/topdee/backend/internal/auth"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
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
		ID:              uuid.NewString(),
		TenantID:        tenant.ID,
		Email:           req.Email,
		PasswordHash:    string(hash),
		Role:            "owner",
		IsPlatformAdmin: h.isBootstrapAdmin(req.Email),
		CreatedAt:       now,
	}
	if _, err := h.mongo.DB.Collection("users").InsertOne(c.Context(), user); err != nil {
		// duplicate email
		if mongo.IsDuplicateKeyError(err) {
			return fiber.NewError(fiber.StatusConflict, "email already registered")
		}
		return err
	}

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

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
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
