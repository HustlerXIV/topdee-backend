package handlers

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/crypto/bcrypt"

	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type SettingsHandler struct {
	mongo *db.Mongo
	cfg   *config.Config
}

func NewSettingsHandler(m *db.Mongo, cfg ...*config.Config) *SettingsHandler {
	h := &SettingsHandler{mongo: m}
	if len(cfg) > 0 {
		h.cfg = cfg[0]
	}
	return h
}

type settingsView struct {
	Account   accountSettingsView   `json:"account"`
	Workspace workspaceSettingsView `json:"workspace"`
}

type accountSettingsView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type workspaceSettingsView struct {
	Name         string `json:"name"`
	Timezone     string `json:"timezone"`
	Website      string `json:"website"`
	BusinessType string `json:"business_type"`
	LogoURL      string `json:"logo_url"`
}

func (h *SettingsHandler) Get(c *fiber.Ctx) error {
	uid := middleware.UserID(c)
	tid := middleware.TenantID(c)

	var u models.User
	if err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"_id": uid, "tenant_id": tid}).Decode(&u); err != nil {
		return err
	}

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}

	return c.JSON(settingsView{
		Account:   accountView(u),
		Workspace: workspaceView(t),
	})
}

type updateAccountReq struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (h *SettingsHandler) UpdateAccount(c *fiber.Ctx) error {
	uid := middleware.UserID(c)
	tid := middleware.TenantID(c)

	var req updateAccountReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	name := strings.TrimSpace(req.Name)
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		return fiber.NewError(fiber.StatusBadRequest, "valid email required")
	}
	if len(name) > 120 {
		return fiber.NewError(fiber.StatusBadRequest, "name too long")
	}

	_, err := h.mongo.DB.Collection("users").UpdateOne(
		c.Context(),
		bson.M{"_id": uid, "tenant_id": tid},
		bson.M{"$set": bson.M{"name": name, "email": email}},
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fiber.NewError(fiber.StatusConflict, "email already registered")
		}
		return err
	}

	var u models.User
	if err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"_id": uid, "tenant_id": tid}).Decode(&u); err != nil {
		return err
	}
	return c.JSON(accountView(u))
}

type updatePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (h *SettingsHandler) UpdatePassword(c *fiber.Ctx) error {
	uid := middleware.UserID(c)
	tid := middleware.TenantID(c)

	var req updatePasswordReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if len(req.NewPassword) < 8 {
		return fiber.NewError(fiber.StatusBadRequest, "new password must be at least 8 characters")
	}

	var u models.User
	if err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"_id": uid, "tenant_id": tid}).Decode(&u); err != nil {
		return err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.CurrentPassword)); err != nil {
		return fiber.NewError(fiber.StatusUnauthorized, "current password is incorrect")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = h.mongo.DB.Collection("users").UpdateOne(
		c.Context(),
		bson.M{"_id": uid, "tenant_id": tid},
		bson.M{"$set": bson.M{"password_hash": string(hash)}},
	)
	if err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

type updateWorkspaceReq struct {
	Name         string `json:"name"`
	Timezone     string `json:"timezone"`
	Website      string `json:"website"`
	BusinessType string `json:"business_type"`
}

func (h *SettingsHandler) UpdateWorkspace(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)

	var req updateWorkspaceReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	name := strings.TrimSpace(req.Name)
	timezone := strings.TrimSpace(req.Timezone)
	website := strings.TrimSpace(req.Website)
	businessType := strings.TrimSpace(req.BusinessType)
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "workspace name required")
	}
	if timezone == "" {
		timezone = "Asia/Bangkok"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid timezone")
	}
	if len(name) > 120 || len(website) > 250 || len(businessType) > 80 {
		return fiber.NewError(fiber.StatusBadRequest, "workspace field too long")
	}

	set := bson.M{
		"name":          name,
		"timezone":      timezone,
		"website":       website,
		"business_type": businessType,
	}
	update := bson.M{"$set": set}
	if timezone != "" {
		update["$set"].(bson.M)["business_hours.timezone"] = timezone
	}

	_, err := h.mongo.DB.Collection("tenants").UpdateOne(
		c.Context(),
		bson.M{"_id": tid},
		update,
	)
	if err != nil {
		return err
	}

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	return c.JSON(workspaceView(t))
}

func accountView(u models.User) accountSettingsView {
	return accountSettingsView{Name: u.Name, Email: u.Email, Role: u.Role}
}

func workspaceView(t models.Tenant) workspaceSettingsView {
	timezone := t.Timezone
	if timezone == "" && t.BusinessHours != nil {
		timezone = t.BusinessHours.Timezone
	}
	if timezone == "" {
		timezone = "Asia/Bangkok"
	}
	return workspaceSettingsView{
		Name:         t.Name,
		Timezone:     timezone,
		Website:      t.Website,
		BusinessType: t.BusinessType,
		LogoURL:      t.LogoURL,
	}
}

// UploadLogo handles POST /api/v1/settings/workspace/logo
// Accepts multipart/form-data with a single "logo" file field.
// Validates type (JPEG/PNG/WebP) and size (max 5 MB), uploads to R2,
// saves the public URL on the tenant document, and returns it.
func (h *SettingsHandler) UploadLogo(c *fiber.Ctx) error {
	if h.cfg == nil || h.cfg.R2AccountID == "" || h.cfg.R2AccessKey == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "logo upload is not configured")
	}

	tid := middleware.TenantID(c)

	file, err := c.FormFile("logo")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "missing logo file (field name: logo)")
	}

	const maxBytes = 5 << 20 // 5 MB
	if file.Size > maxBytes {
		return fiber.NewError(fiber.StatusBadRequest, "logo must be ≤ 5 MB")
	}

	ct := file.Header.Get("Content-Type")
	ext := ""
	switch ct {
	case "image/jpeg":
		ext = "jpg"
	case "image/png":
		ext = "png"
	case "image/webp":
		ext = "webp"
	default:
		// Fall back to file extension if content-type is unreliable.
		switch strings.ToLower(filepath.Ext(file.Filename)) {
		case ".jpg", ".jpeg":
			ext, ct = "jpg", "image/jpeg"
		case ".png":
			ext, ct = "png", "image/png"
		case ".webp":
			ext, ct = "webp", "image/webp"
		default:
			return fiber.NewError(fiber.StatusBadRequest, "logo must be JPEG, PNG, or WebP")
		}
	}

	f, err := file.Open()
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	defer f.Close()

	body, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read upload: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return fiber.NewError(fiber.StatusBadRequest, "logo must be ≤ 5 MB")
	}

	key := fmt.Sprintf("logos/%s/%s.%s", tid, uuid.New().String(), ext)

	client := &r2Client{
		accountID: h.cfg.R2AccountID,
		accessKey: h.cfg.R2AccessKey,
		secretKey: h.cfg.R2SecretKey,
		bucket:    h.cfg.R2Bucket,
		publicURL: h.cfg.R2PublicURL,
	}
	logoURL, err := client.PutObject(key, ct, body)
	if err != nil {
		return fmt.Errorf("r2 upload: %w", err)
	}

	_, err = h.mongo.DB.Collection("tenants").UpdateOne(
		c.Context(),
		bson.M{"_id": tid},
		bson.M{"$set": bson.M{"logo_url": logoURL}},
	)
	if err != nil {
		return fmt.Errorf("save logo_url: %w", err)
	}

	return c.JSON(fiber.Map{"logo_url": logoURL})
}
