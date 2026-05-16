package handlers

// Team management — members + invites.
//
// Permission model (kept simple on purpose):
//
//   owner  → everything; only created by /auth/register; can promote/demote
//            anyone, can delete the workspace later.
//   admin  → invite, change non-owner roles, remove non-owner members.
//   agent  → reply to chats, view dashboards.
//   viewer → read-only.
//
// Owners are immutable: you cannot demote or remove an owner via this API.
// (We assume one owner per workspace for now; ownership transfer is a future
// feature.)

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/crypto/bcrypt"

	"github.com/topdee/backend/internal/auth"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/email"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type TeamHandler struct {
	mongo  *db.Mongo
	cfg    *config.Config
	mailer *email.Mailer
}

func NewTeamHandler(m *db.Mongo, cfg *config.Config) *TeamHandler {
	return &TeamHandler{
		mongo: m,
		cfg:   cfg,
		mailer: &email.Mailer{
			APIKey: cfg.ResendAPIKey,
			From:   cfg.EmailFrom,
		},
	}
}

// ── Members ────────────────────────────────────────────────────────────

// GET /api/v1/team/members
func (h *TeamHandler) ListMembers(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	cur, err := h.mongo.DB.Collection("users").
		Find(c.Context(), bson.M{"tenant_id": tid})
	if err != nil {
		return err
	}
	var users []models.User
	if err := cur.All(c.Context(), &users); err != nil {
		return err
	}
	if users == nil {
		users = []models.User{}
	}
	return c.JSON(users)
}

type updateRoleReq struct {
	Role string `json:"role"`
}

// PATCH /api/v1/team/members/:id — change role.
//
// Owner-only. Cannot change another owner's role; cannot promote anyone TO
// owner (ownership transfer would need its own dedicated endpoint).
func (h *TeamHandler) UpdateMemberRole(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := c.Params("id")

	var req updateRoleReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if !auth.IsValidRole(req.Role) || req.Role == auth.RoleOwner {
		return fiber.NewError(fiber.StatusBadRequest, "role must be admin, agent, or viewer")
	}

	// Load the target user; ensure tenant scope and not an owner.
	var target models.User
	if err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"_id": uid, "tenant_id": tid}).Decode(&target); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "member not found")
	}
	if target.Role == auth.RoleOwner {
		return fiber.NewError(fiber.StatusForbidden, "cannot change owner's role")
	}

	_, err := h.mongo.DB.Collection("users").UpdateOne(
		c.Context(),
		bson.M{"_id": uid, "tenant_id": tid},
		bson.M{"$set": bson.M{"role": req.Role}},
	)
	if err != nil {
		return err
	}
	target.Role = req.Role
	return c.JSON(target)
}

// DELETE /api/v1/team/members/:id — remove a member.
//
// Allowed for owner+admin. Owners cannot be removed. Admins cannot remove
// other admins (only the owner can); kept simple by checking against owner role.
func (h *TeamHandler) RemoveMember(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	currentRole := middleware.Role(c)
	uid := c.Params("id")

	if uid == middleware.UserID(c) {
		return fiber.NewError(fiber.StatusBadRequest, "cannot remove yourself; transfer ownership first")
	}

	var target models.User
	if err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"_id": uid, "tenant_id": tid}).Decode(&target); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "member not found")
	}
	if target.Role == auth.RoleOwner {
		return fiber.NewError(fiber.StatusForbidden, "cannot remove the workspace owner")
	}
	if target.Role == auth.RoleAdmin && currentRole != auth.RoleOwner {
		return fiber.NewError(fiber.StatusForbidden, "only the owner can remove an admin")
	}

	_, err := h.mongo.DB.Collection("users").DeleteOne(
		c.Context(),
		bson.M{"_id": uid, "tenant_id": tid},
	)
	if err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Invites ────────────────────────────────────────────────────────────

const inviteTTLDays = 7

// GET /api/v1/team/invites — pending invites only (owner+admin).
func (h *TeamHandler) ListInvites(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	cur, err := h.mongo.DB.Collection("team_invites").Find(
		c.Context(),
		bson.M{"tenant_id": tid, "status": models.InviteStatusPending},
	)
	if err != nil {
		return err
	}
	var out []models.TeamInvite
	if err := cur.All(c.Context(), &out); err != nil {
		return err
	}
	if out == nil {
		out = []models.TeamInvite{}
	}
	return c.JSON(out)
}

type createInviteReq struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type createInviteResp struct {
	Invite     models.TeamInvite `json:"invite"`
	AcceptURL  string            `json:"accept_url"`
}

// POST /api/v1/team/invites — create a new pending invite and email the recipient.
func (h *TeamHandler) CreateInvite(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	by := middleware.UserID(c)

	var req createInviteReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		return fiber.NewError(fiber.StatusBadRequest, "valid email required")
	}
	if !auth.IsValidRole(req.Role) || req.Role == auth.RoleOwner {
		return fiber.NewError(fiber.StatusBadRequest, "role must be admin, agent, or viewer")
	}

	// Reject if a user with that email already exists in this tenant.
	count, err := h.mongo.DB.Collection("users").CountDocuments(c.Context(),
		bson.M{"tenant_id": tid, "email": req.Email})
	if err != nil {
		return err
	}
	if count > 0 {
		return fiber.NewError(fiber.StatusConflict, "this email already belongs to a member")
	}

	// Look up workspace name for the email subject + body.
	var tenant models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&tenant); err != nil {
		return err
	}

	// Look up the inviter's email for the "invited by" line.
	var inviter models.User
	_ = h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"_id": by}).Decode(&inviter)
	inviterEmail := inviter.Email
	if inviterEmail == "" {
		inviterEmail = middleware.Email(c)
	}

	// Replace any existing pending invite for the same email — saves admins
	// from chasing duplicates.
	_, _ = h.mongo.DB.Collection("team_invites").UpdateMany(
		c.Context(),
		bson.M{"tenant_id": tid, "email": req.Email, "status": models.InviteStatusPending},
		bson.M{"$set": bson.M{"status": models.InviteStatusRevoked}},
	)

	now := time.Now().UTC()
	invite := models.TeamInvite{
		ID:        uuid.NewString(),
		TenantID:  tid,
		Email:     req.Email,
		Role:      req.Role,
		Token:     newInviteToken(),
		Status:    models.InviteStatusPending,
		InvitedBy: by,
		CreatedAt: now,
		ExpiresAt: now.AddDate(0, 0, inviteTTLDays),
	}
	if _, err := h.mongo.DB.Collection("team_invites").InsertOne(c.Context(), invite); err != nil {
		return err
	}

	acceptURL := h.cfg.AcceptInviteBaseURL + "?token=" + invite.Token

	// Send invite email — non-fatal: the accept_url is always returned in the
	// response so the admin can share it manually if email isn't configured.
	go func() {
		subject := fmt.Sprintf("You're invited to join %s on Topdee", tenant.Name)
		html := email.InviteHTML(tenant.Name, inviterEmail, acceptURL, invite.ExpiresAt)
		if err := h.mailer.Send(invite.Email, subject, html); err != nil {
			log.Printf("[invite] email to %s failed: %v", invite.Email, err)
		} else {
			log.Printf("[invite] email sent to %s", invite.Email)
		}
	}()

	return c.Status(fiber.StatusCreated).JSON(createInviteResp{
		Invite:    invite,
		AcceptURL: acceptURL,
	})
}

// DELETE /api/v1/team/invites/:id — revoke a pending invite.
func (h *TeamHandler) RevokeInvite(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	id := c.Params("id")
	res, err := h.mongo.DB.Collection("team_invites").UpdateOne(
		c.Context(),
		bson.M{"_id": id, "tenant_id": tid, "status": models.InviteStatusPending},
		bson.M{"$set": bson.M{"status": models.InviteStatusRevoked}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return fiber.NewError(fiber.StatusNotFound, "invite not found or already used")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// POST /api/v1/team/invites/:id/resend — bump expiry, re-send email, return fresh URL.
func (h *TeamHandler) ResendInvite(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	id := c.Params("id")

	var inv models.TeamInvite
	if err := h.mongo.DB.Collection("team_invites").
		FindOne(c.Context(), bson.M{"_id": id, "tenant_id": tid}).Decode(&inv); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "invite not found")
	}
	if inv.Status != models.InviteStatusPending && inv.Status != models.InviteStatusExpired {
		return fiber.NewError(fiber.StatusBadRequest, "invite is "+inv.Status+"; revoke and create a new one")
	}

	inv.Token = newInviteToken()
	inv.Status = models.InviteStatusPending
	inv.ExpiresAt = time.Now().UTC().AddDate(0, 0, inviteTTLDays)

	_, err := h.mongo.DB.Collection("team_invites").UpdateOne(
		c.Context(),
		bson.M{"_id": id},
		bson.M{"$set": bson.M{
			"token":      inv.Token,
			"status":     inv.Status,
			"expires_at": inv.ExpiresAt,
		}},
	)
	if err != nil {
		return err
	}

	acceptURL := h.cfg.AcceptInviteBaseURL + "?token=" + inv.Token

	// Capture all values from the Fiber context NOW, before the handler
	// returns and Fiber recycles c. Accessing c inside a goroutine is a
	// data race / nil-pointer panic.
	inviterEmail := middleware.Email(c)
	invEmail := inv.Email
	invExpiresAt := inv.ExpiresAt

	// Re-send email — look up workspace name for the template.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var tenant models.Tenant
		if err := h.mongo.DB.Collection("tenants").
			FindOne(ctx, bson.M{"_id": tid}).Decode(&tenant); err != nil {
			return
		}
		subject := fmt.Sprintf("You're invited to join %s on Topdee", tenant.Name)
		html := email.InviteHTML(tenant.Name, inviterEmail, acceptURL, invExpiresAt)
		if err := h.mailer.Send(invEmail, subject, html); err != nil {
			log.Printf("[invite] resend email to %s failed: %v", invEmail, err)
		} else {
			log.Printf("[invite] resend email sent to %s", invEmail)
		}
	}()

	return c.JSON(createInviteResp{
		Invite:    inv,
		AcceptURL: acceptURL,
	})
}

// ── Invite info (public) ───────────────────────────────────────────────

type inviteInfoResp struct {
	Email         string `json:"email"`
	WorkspaceName string `json:"workspace_name"`
	InviterEmail  string `json:"inviter_email"`
	ExpiresAt     string `json:"expires_at"`
}

// GET /api/v1/auth/invite-info?token=... — public, no auth.
//
// Returns just enough metadata for the accept-invite page to show "you were
// invited to <workspace> as <email>" before the user fills in their password.
func (h *TeamHandler) InviteInfo(c *fiber.Ctx) error {
	token := c.Query("token")
	if token == "" {
		return fiber.NewError(fiber.StatusBadRequest, "token required")
	}

	var inv models.TeamInvite
	if err := h.mongo.DB.Collection("team_invites").
		FindOne(c.Context(), bson.M{"token": token}).Decode(&inv); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "invite not found")
	}
	if inv.Status != models.InviteStatusPending {
		return fiber.NewError(fiber.StatusGone, "invite is "+inv.Status)
	}
	if time.Now().UTC().After(inv.ExpiresAt) {
		return fiber.NewError(fiber.StatusGone, "invite expired")
	}

	// Look up workspace name + inviter email for display.
	var tenant models.Tenant
	_ = h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": inv.TenantID}).Decode(&tenant)

	var inviter models.User
	_ = h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"_id": inv.InvitedBy}).Decode(&inviter)

	return c.JSON(inviteInfoResp{
		Email:         inv.Email,
		WorkspaceName: tenant.Name,
		InviterEmail:  inviter.Email,
		ExpiresAt:     inv.ExpiresAt.Format("2 January 2006"),
	})
}

// ── Accept ─────────────────────────────────────────────────────────────

type acceptInviteReq struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// POST /api/v1/auth/accept-invite — public.
//
// Trades a valid invite token + name + password for a freshly-minted user
// in the inviter's tenant + a JWT. The JWT is scoped to the same tenant the
// invite belonged to, so the new member lands in the right workspace.
func (h *TeamHandler) AcceptInvite(c *fiber.Ctx) error {
	var req acceptInviteReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.Token == "" || req.Name == "" || len(req.Password) < 8 {
		return fiber.NewError(fiber.StatusBadRequest, "token, name, and password (>=8) required")
	}

	var inv models.TeamInvite
	if err := h.mongo.DB.Collection("team_invites").
		FindOne(c.Context(), bson.M{"token": req.Token}).Decode(&inv); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "invite not found")
	}

	if inv.Status != models.InviteStatusPending {
		return fiber.NewError(fiber.StatusGone, "invite is "+inv.Status)
	}
	if time.Now().UTC().After(inv.ExpiresAt) {
		_, _ = h.mongo.DB.Collection("team_invites").UpdateOne(c.Context(),
			bson.M{"_id": inv.ID},
			bson.M{"$set": bson.M{"status": models.InviteStatusExpired}})
		return fiber.NewError(fiber.StatusGone, "invite expired")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	user := models.User{
		ID:           uuid.NewString(),
		TenantID:     inv.TenantID,
		Name:         strings.TrimSpace(req.Name),
		Email:        inv.Email,
		PasswordHash: string(hash),
		Role:         inv.Role,
		CreatedAt:    now,
	}
	if _, err := h.mongo.DB.Collection("users").InsertOne(c.Context(), user); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fiber.NewError(fiber.StatusConflict, "an account with this email already exists")
		}
		return err
	}

	// Mark the invite consumed.
	_, _ = h.mongo.DB.Collection("team_invites").UpdateOne(c.Context(),
		bson.M{"_id": inv.ID},
		bson.M{"$set": bson.M{
			"status":      models.InviteStatusAccepted,
			"accepted_at": now,
		}})

	token, err := auth.IssueToken(auth.IssueOpts{
		Secret: h.cfg.JWTSecret, UserID: user.ID, TenantID: user.TenantID,
		Email: user.Email, Role: user.Role, IsAdmin: false,
		TTLHours: h.cfg.JWTTTLHours,
	})
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"token": token, "user": user})
}

// ── helpers ────────────────────────────────────────────────────────────

// newInviteToken returns a 32-byte URL-safe random string.
func newInviteToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a UUID — still unique and unguessable enough for a
		// token that will be rotated/revoked frequently.
		return strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
