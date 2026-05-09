package handlers

// Platform-admin endpoints. Distinct from /team — these are operated by
// Topdee staff and span every tenant.
//
// Routes (all under /api/v1/admin, all gated by middleware.RequireAdmin):
//
//   GET    /metrics                       — high-level counts
//   GET    /tenants                       — list, optional ?q= search
//   GET    /tenants/:id                   — full record + member count
//   PATCH  /tenants/:id                   — { plan?, suspended? }
//   DELETE /tenants/:id                   — cascade-deletes users + KBs + messages
//   GET    /users                         — list, ?tenant_id=, ?q=, ?suspended=
//   PATCH  /users/:id                     — { role?, suspended?, is_platform_admin? }
//   DELETE /users/:id                     — removes a single user (must not be last owner)
//   GET    /plans                         — list all plans
//   POST   /plans                         — create a plan
//   GET    /plans/:id                     — get one plan
//   PUT    /plans/:id                     — full update
//   DELETE /plans/:id                     — delete (blocked if tenants use it)
//
// All write operations are idempotent enough to be safely retried.

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/topdee/backend/internal/auth"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type AdminHandler struct {
	mongo *db.Mongo
}

func NewAdminHandler(m *db.Mongo) *AdminHandler {
	return &AdminHandler{mongo: m}
}

// ── Metrics ────────────────────────────────────────────────────────────

type metricsResp struct {
	Tenants struct {
		Total     int64 `json:"total"`
		Suspended int64 `json:"suspended"`
		ByPlan    map[string]int64 `json:"by_plan"`
	} `json:"tenants"`
	Users struct {
		Total     int64 `json:"total"`
		Suspended int64 `json:"suspended"`
		Admins    int64 `json:"admins"`
	} `json:"users"`
	Messages struct {
		Total int64 `json:"total"`
	} `json:"messages"`
	KnowledgeBases struct {
		Total  int64 `json:"total"`
		Chunks int64 `json:"chunks"`
	} `json:"knowledge_bases"`
}

// GET /api/v1/admin/metrics
func (h *AdminHandler) Metrics(c *fiber.Ctx) error {
	ctx := c.Context()
	out := metricsResp{}
	out.Tenants.ByPlan = map[string]int64{}

	// Tenants
	out.Tenants.Total, _ = h.mongo.DB.Collection("tenants").CountDocuments(ctx, bson.M{})
	out.Tenants.Suspended, _ = h.mongo.DB.Collection("tenants").CountDocuments(ctx, bson.M{"suspended": true})

	// By plan — small aggregate, capped by the very small set of plans.
	cur, err := h.mongo.DB.Collection("tenants").Aggregate(ctx, []bson.M{
		{"$group": bson.M{"_id": "$plan", "n": bson.M{"$sum": 1}}},
	})
	if err == nil {
		var rows []struct {
			ID string `bson:"_id"`
			N  int64  `bson:"n"`
		}
		_ = cur.All(ctx, &rows)
		for _, r := range rows {
			out.Tenants.ByPlan[r.ID] = r.N
		}
	}

	// Users
	out.Users.Total, _ = h.mongo.DB.Collection("users").CountDocuments(ctx, bson.M{})
	out.Users.Suspended, _ = h.mongo.DB.Collection("users").CountDocuments(ctx, bson.M{"suspended": true})
	out.Users.Admins, _ = h.mongo.DB.Collection("users").CountDocuments(ctx, bson.M{"is_platform_admin": true})

	// Messages + KB stats
	out.Messages.Total, _ = h.mongo.DB.Collection("messages").CountDocuments(ctx, bson.M{})
	out.KnowledgeBases.Total, _ = h.mongo.DB.Collection("knowledge_bases").CountDocuments(ctx, bson.M{})

	// Sum chunk counts across all KBs
	cur2, err := h.mongo.DB.Collection("knowledge_bases").Aggregate(ctx, []bson.M{
		{"$group": bson.M{"_id": nil, "n": bson.M{"$sum": "$chunk_count"}}},
	})
	if err == nil {
		var rows []struct {
			N int64 `bson:"n"`
		}
		_ = cur2.All(ctx, &rows)
		if len(rows) > 0 {
			out.KnowledgeBases.Chunks = rows[0].N
		}
	}

	return c.JSON(out)
}

// ── Tenants ────────────────────────────────────────────────────────────

// adminTenantView strips fields admins don't need (FB token, LINE secrets)
// but exposes everything they do, including subscription state.
type adminTenantView struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Plan         string                `json:"plan"`
	UsageTokens  int64                 `json:"usage_tokens"`
	Suspended    bool                  `json:"suspended"`
	MemberCount  int64                 `json:"member_count"`
	Subscription *models.Subscription  `json:"subscription,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
}

func toAdminTenantView(t models.Tenant, memberCount int64) adminTenantView {
	return adminTenantView{
		ID: t.ID, Name: t.Name, Plan: t.Plan, UsageTokens: t.UsageTokens,
		Suspended: t.Suspended, MemberCount: memberCount,
		Subscription: t.Subscription, CreatedAt: t.CreatedAt,
	}
}

// GET /api/v1/admin/tenants?q=foo
func (h *AdminHandler) ListTenants(c *fiber.Ctx) error {
	ctx := c.Context()
	q := strings.TrimSpace(c.Query("q"))

	filter := bson.M{}
	if q != "" {
		// Simple case-insensitive name search; small datasets so a regex is fine.
		filter["name"] = bson.M{"$regex": q, "$options": "i"}
	}

	cur, err := h.mongo.DB.Collection("tenants").Find(
		ctx, filter,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(200),
	)
	if err != nil {
		return err
	}
	var tenants []models.Tenant
	if err := cur.All(ctx, &tenants); err != nil {
		return err
	}

	// One member-count batch — projecting just the tenant_id field and
	// counting client-side keeps Mongo round-trips to a single query.
	memberCounts := map[string]int64{}
	if len(tenants) > 0 {
		cur2, err := h.mongo.DB.Collection("users").Aggregate(ctx, []bson.M{
			{"$group": bson.M{"_id": "$tenant_id", "n": bson.M{"$sum": 1}}},
		})
		if err == nil {
			var rows []struct {
				ID string `bson:"_id"`
				N  int64  `bson:"n"`
			}
			_ = cur2.All(ctx, &rows)
			for _, r := range rows {
				memberCounts[r.ID] = r.N
			}
		}
	}

	out := make([]adminTenantView, 0, len(tenants))
	for _, t := range tenants {
		out = append(out, toAdminTenantView(t, memberCounts[t.ID]))
	}
	return c.JSON(out)
}

// GET /api/v1/admin/tenants/:id
func (h *AdminHandler) GetTenant(c *fiber.Ctx) error {
	id := c.Params("id")
	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": id}).Decode(&t); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "tenant not found")
	}
	count, _ := h.mongo.DB.Collection("users").CountDocuments(c.Context(),
		bson.M{"tenant_id": id})
	return c.JSON(toAdminTenantView(t, count))
}

type updateTenantReq struct {
	Plan      *string `json:"plan,omitempty"`
	Suspended *bool   `json:"suspended,omitempty"`
}

// PATCH /api/v1/admin/tenants/:id
func (h *AdminHandler) UpdateTenant(c *fiber.Ctx) error {
	id := c.Params("id")
	var req updateTenantReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	set := bson.M{}
	if req.Plan != nil {
		switch *req.Plan {
		case "free", "starter", "growth", "pro", "enterprise":
			set["plan"] = *req.Plan
		default:
			return fiber.NewError(fiber.StatusBadRequest, "invalid plan")
		}
	}
	if req.Suspended != nil {
		set["suspended"] = *req.Suspended
	}
	if len(set) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "nothing to update")
	}

	_, err := h.mongo.DB.Collection("tenants").UpdateOne(c.Context(),
		bson.M{"_id": id}, bson.M{"$set": set})
	if err != nil {
		return err
	}
	// Re-fetch + return the merged view so the UI can rehydrate without
	// having to merge the patch client-side.
	return h.GetTenant(c)
}

// ── Subscription ───────────────────────────────────────────────────────

// validSubStatuses guards against typos in the status field.
var validSubStatuses = map[string]bool{
	models.SubStatusTrialing: true,
	models.SubStatusActive:   true,
	models.SubStatusPastDue:  true,
	models.SubStatusCanceled: true,
	models.SubStatusPaused:   true,
}

type updateSubscriptionReq struct {
	Status            *string    `json:"status,omitempty"`
	TrialEndsAt       *time.Time `json:"trial_ends_at,omitempty"`
	CurrentPeriodEnd  *time.Time `json:"current_period_end,omitempty"`
	CanceledAt        *time.Time `json:"canceled_at,omitempty"`
	CancelAtPeriodEnd *bool      `json:"cancel_at_period_end,omitempty"`
	AdminNotes        *string    `json:"admin_notes,omitempty"`
	// Sentinel: if true, blanks the corresponding date field. JSON's null
	// can't reliably round-trip through Go's *time.Time decode, so we use
	// explicit clear flags instead.
	ClearTrialEndsAt      bool `json:"clear_trial_ends_at,omitempty"`
	ClearCurrentPeriodEnd bool `json:"clear_current_period_end,omitempty"`
	ClearCanceledAt       bool `json:"clear_canceled_at,omitempty"`
}

// PATCH /api/v1/admin/tenants/:id/subscription
//
// Partial update — only fields present in the body are touched. Always
// returns the updated subscription so the UI can re-hydrate.
func (h *AdminHandler) UpdateSubscription(c *fiber.Ctx) error {
	id := c.Params("id")
	var req updateSubscriptionReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}

	// Load existing or seed a new one with sensible defaults.
	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": id}).Decode(&t); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "tenant not found")
	}
	sub := t.Subscription
	if sub == nil {
		sub = &models.Subscription{Status: models.SubStatusActive}
	}

	if req.Status != nil {
		if !validSubStatuses[*req.Status] {
			return fiber.NewError(fiber.StatusBadRequest, "invalid status")
		}
		sub.Status = *req.Status
		// Auto-stamp canceled_at when transitioning into canceled and the
		// caller didn't supply one.
		if *req.Status == models.SubStatusCanceled && sub.CanceledAt == nil && !req.ClearCanceledAt {
			now := time.Now().UTC()
			sub.CanceledAt = &now
		}
	}
	if req.TrialEndsAt != nil {
		sub.TrialEndsAt = req.TrialEndsAt
	}
	if req.ClearTrialEndsAt {
		sub.TrialEndsAt = nil
	}
	if req.CurrentPeriodEnd != nil {
		sub.CurrentPeriodEnd = req.CurrentPeriodEnd
	}
	if req.ClearCurrentPeriodEnd {
		sub.CurrentPeriodEnd = nil
	}
	if req.CanceledAt != nil {
		sub.CanceledAt = req.CanceledAt
	}
	if req.ClearCanceledAt {
		sub.CanceledAt = nil
	}
	if req.CancelAtPeriodEnd != nil {
		sub.CancelAtPeriodEnd = *req.CancelAtPeriodEnd
	}
	if req.AdminNotes != nil {
		if len(*req.AdminNotes) > 4000 {
			return fiber.NewError(fiber.StatusBadRequest, "admin_notes too long")
		}
		sub.AdminNotes = *req.AdminNotes
	}
	sub.UpdatedAt = time.Now().UTC()

	_, err := h.mongo.DB.Collection("tenants").UpdateOne(c.Context(),
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"subscription": sub}})
	if err != nil {
		return err
	}
	return c.JSON(sub)
}

type extendSubReq struct {
	Days int `json:"days"`
}

// POST /api/v1/admin/tenants/:id/subscription/extend
//
// One-click "give them N more days" — bumps current_period_end by N days
// (or trial_ends_at if currently trialing). Convenient for "we processed
// the bank transfer, extend by 30 days" workflows.
func (h *AdminHandler) ExtendSubscription(c *fiber.Ctx) error {
	id := c.Params("id")
	var req extendSubReq
	if err := c.BodyParser(&req); err != nil || req.Days <= 0 || req.Days > 365 {
		return fiber.NewError(fiber.StatusBadRequest, "days must be between 1 and 365")
	}

	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": id}).Decode(&t); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "tenant not found")
	}
	sub := t.Subscription
	if sub == nil {
		sub = &models.Subscription{Status: models.SubStatusActive}
	}

	now := time.Now().UTC()
	addDays := time.Duration(req.Days) * 24 * time.Hour

	if sub.Status == models.SubStatusTrialing {
		base := now
		if sub.TrialEndsAt != nil && sub.TrialEndsAt.After(now) {
			base = *sub.TrialEndsAt
		}
		end := base.Add(addDays)
		sub.TrialEndsAt = &end
	} else {
		base := now
		if sub.CurrentPeriodEnd != nil && sub.CurrentPeriodEnd.After(now) {
			base = *sub.CurrentPeriodEnd
		}
		end := base.Add(addDays)
		sub.CurrentPeriodEnd = &end
		// Coming back from past_due/canceled → active.
		if sub.Status == models.SubStatusPastDue || sub.Status == models.SubStatusCanceled {
			sub.Status = models.SubStatusActive
			sub.CanceledAt = nil
			sub.CancelAtPeriodEnd = false
		}
	}
	sub.UpdatedAt = now

	_, err := h.mongo.DB.Collection("tenants").UpdateOne(c.Context(),
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"subscription": sub}})
	if err != nil {
		return err
	}
	return c.JSON(sub)
}

// DELETE /api/v1/admin/tenants/:id — cascade.
//
// Also removes users, knowledge bases, messages, team invites tied to the
// tenant. The Qdrant vectors are NOT deleted here (the AI service owns
// that lifecycle); this is a best-effort cleanup of the Mongo side.
func (h *AdminHandler) DeleteTenant(c *fiber.Ctx) error {
	id := c.Params("id")
	ctx := c.Context()

	for _, coll := range []string{"users", "knowledge_bases", "messages", "team_invites"} {
		_, _ = h.mongo.DB.Collection(coll).DeleteMany(ctx, bson.M{"tenant_id": id})
	}
	_, err := h.mongo.DB.Collection("tenants").DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Users ──────────────────────────────────────────────────────────────

// GET /api/v1/admin/users?tenant_id=&q=&suspended=true
func (h *AdminHandler) ListUsers(c *fiber.Ctx) error {
	ctx := c.Context()
	filter := bson.M{}
	if tid := strings.TrimSpace(c.Query("tenant_id")); tid != "" {
		filter["tenant_id"] = tid
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		filter["$or"] = []bson.M{
			{"email": bson.M{"$regex": q, "$options": "i"}},
			{"name": bson.M{"$regex": q, "$options": "i"}},
		}
	}
	if susp := c.Query("suspended"); susp == "true" {
		filter["suspended"] = true
	}

	cur, err := h.mongo.DB.Collection("users").Find(
		ctx, filter,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(500),
	)
	if err != nil {
		return err
	}
	var users []models.User
	if err := cur.All(ctx, &users); err != nil {
		return err
	}
	if users == nil {
		users = []models.User{}
	}
	return c.JSON(users)
}

type updateUserReq struct {
	Role            *string `json:"role,omitempty"`
	Suspended       *bool   `json:"suspended,omitempty"`
	IsPlatformAdmin *bool   `json:"is_platform_admin,omitempty"`
}

// PATCH /api/v1/admin/users/:id
//
// Cannot self-demote: an admin removing their own platform-admin flag is
// rejected to prevent locking yourself out by accident.
func (h *AdminHandler) UpdateUser(c *fiber.Ctx) error {
	id := c.Params("id")
	me := middleware.UserID(c)

	var req updateUserReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	set := bson.M{}
	if req.Role != nil {
		if !auth.IsValidRole(*req.Role) {
			return fiber.NewError(fiber.StatusBadRequest, "invalid role")
		}
		set["role"] = *req.Role
	}
	if req.Suspended != nil {
		if id == me && *req.Suspended {
			return fiber.NewError(fiber.StatusBadRequest, "cannot suspend yourself")
		}
		set["suspended"] = *req.Suspended
	}
	if req.IsPlatformAdmin != nil {
		if id == me && !*req.IsPlatformAdmin {
			return fiber.NewError(fiber.StatusBadRequest, "cannot remove your own admin flag")
		}
		set["is_platform_admin"] = *req.IsPlatformAdmin
	}
	if len(set) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "nothing to update")
	}

	_, err := h.mongo.DB.Collection("users").UpdateOne(c.Context(),
		bson.M{"_id": id}, bson.M{"$set": set})
	if err != nil {
		return err
	}
	var u models.User
	if err := h.mongo.DB.Collection("users").
		FindOne(c.Context(), bson.M{"_id": id}).Decode(&u); err != nil {
		return err
	}
	return c.JSON(u)
}

// DELETE /api/v1/admin/users/:id
//
// Refuses to delete the sole owner of a tenant (would leave a workspace
// with no one in charge). Use DeleteTenant for that.
func (h *AdminHandler) DeleteUser(c *fiber.Ctx) error {
	id := c.Params("id")
	ctx := c.Context()
	if id == middleware.UserID(c) {
		return fiber.NewError(fiber.StatusBadRequest, "cannot delete yourself")
	}
	var u models.User
	if err := h.mongo.DB.Collection("users").
		FindOne(ctx, bson.M{"_id": id}).Decode(&u); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "user not found")
	}
	if u.Role == auth.RoleOwner {
		count, _ := h.mongo.DB.Collection("users").CountDocuments(ctx, bson.M{
			"tenant_id": u.TenantID, "role": auth.RoleOwner,
		})
		if count <= 1 {
			return fiber.NewError(fiber.StatusBadRequest,
				"cannot delete the sole owner — delete the tenant instead")
		}
	}
	_, err := h.mongo.DB.Collection("users").DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Plans ──────────────────────────────────────────────────────────────

// PublicPlans is a no-auth handler for GET /api/v1/plans.
// Returns only active, public plans, sorted by sort_order.
// Plans with is_public=false are hidden/custom plans only visible to admins.
func PublicPlans(m *db.Mongo) fiber.Handler {
	return func(c *fiber.Ctx) error {
		cur, err := m.DB.Collection("plans").Find(
			c.Context(),
			bson.M{"is_active": true, "is_public": true},
			options.Find().SetSort(bson.D{{Key: "sort_order", Value: 1}}),
		)
		if err != nil {
			return err
		}
		var plans []models.Plan
		if err := cur.All(c.Context(), &plans); err != nil {
			return err
		}
		if plans == nil {
			plans = []models.Plan{}
		}
		return c.JSON(plans)
	}
}

// GET /api/v1/admin/plans
func (h *AdminHandler) ListPlans(c *fiber.Ctx) error {
	cur, err := h.mongo.DB.Collection("plans").Find(
		c.Context(), bson.M{},
		options.Find().SetSort(bson.D{{Key: "sort_order", Value: 1}}),
	)
	if err != nil {
		return err
	}
	var plans []models.Plan
	if err := cur.All(c.Context(), &plans); err != nil {
		return err
	}
	if plans == nil {
		plans = []models.Plan{}
	}
	return c.JSON(plans)
}

// GET /api/v1/admin/plans/:id
func (h *AdminHandler) GetPlan(c *fiber.Ctx) error {
	id := c.Params("id")
	var plan models.Plan
	if err := h.mongo.DB.Collection("plans").
		FindOne(c.Context(), bson.M{"_id": id}).Decode(&plan); err != nil {
		return fiber.NewError(fiber.StatusNotFound, "plan not found")
	}
	return c.JSON(plan)
}

type planReq struct {
	ID            string            `json:"id"`
	DisplayName   string            `json:"display_name"`
	Description   string            `json:"description"`
	Price         float64           `json:"price"`
	Currency      string            `json:"currency"`
	IsActive      bool              `json:"is_active"`
	IsPublic      bool              `json:"is_public"`
	IsRecommended bool              `json:"is_recommended"`
	SortOrder     int               `json:"sort_order"`
	ExpiryDays    int               `json:"expiry_days"`
	Limits        models.PlanLimits `json:"limits"`
}

// POST /api/v1/admin/plans
func (h *AdminHandler) CreatePlan(c *fiber.Ctx) error {
	var req planReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}
	if req.DisplayName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "display_name is required")
	}
	if req.Currency == "" {
		req.Currency = "THB"
	}
	if req.Limits.Channels == nil {
		req.Limits.Channels = map[string]int{}
	}

	now := time.Now().UTC()
	plan := models.Plan{
		ID:            strings.ToLower(strings.TrimSpace(req.ID)),
		DisplayName:   req.DisplayName,
		Description:   req.Description,
		Price:         req.Price,
		Currency:      req.Currency,
		IsActive:      req.IsActive,
		IsPublic:      req.IsPublic,
		IsRecommended: req.IsRecommended,
		SortOrder:     req.SortOrder,
		ExpiryDays:    req.ExpiryDays,
		Limits:        req.Limits,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if _, err := h.mongo.DB.Collection("plans").InsertOne(c.Context(), plan); err != nil {
		return fiber.NewError(fiber.StatusConflict, "plan id already exists")
	}
	return c.Status(fiber.StatusCreated).JSON(plan)
}

// PUT /api/v1/admin/plans/:id
func (h *AdminHandler) UpdatePlan(c *fiber.Ctx) error {
	id := c.Params("id")
	var req planReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.DisplayName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "display_name is required")
	}
	if req.Currency == "" {
		req.Currency = "THB"
	}
	if req.Limits.Channels == nil {
		req.Limits.Channels = map[string]int{}
	}

	set := bson.M{
		"display_name":   req.DisplayName,
		"description":    req.Description,
		"price":          req.Price,
		"currency":       req.Currency,
		"is_active":      req.IsActive,
		"is_public":      req.IsPublic,
		"is_recommended": req.IsRecommended,
		"sort_order":     req.SortOrder,
		"expiry_days":    req.ExpiryDays,
		"limits":         req.Limits,
		"updated_at":     time.Now().UTC(),
	}
	res, err := h.mongo.DB.Collection("plans").UpdateOne(
		c.Context(), bson.M{"_id": id}, bson.M{"$set": set},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return fiber.NewError(fiber.StatusNotFound, "plan not found")
	}
	return h.GetPlan(c)
}

// DELETE /api/v1/admin/plans/:id
// Blocked if any tenant is currently on this plan.
func (h *AdminHandler) DeletePlan(c *fiber.Ctx) error {
	id := c.Params("id")
	count, _ := h.mongo.DB.Collection("tenants").CountDocuments(
		c.Context(), bson.M{"plan": id},
	)
	if count > 0 {
		return fiber.NewError(fiber.StatusConflict,
			"cannot delete: "+string(rune('0'+count))+" tenant(s) are on this plan")
	}
	res, err := h.mongo.DB.Collection("plans").DeleteOne(c.Context(), bson.M{"_id": id})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return fiber.NewError(fiber.StatusNotFound, "plan not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// SeedDefaultPlans inserts the built-in plan tiers if the plans collection
// is empty. Safe to call on every startup — it's a no-op when plans exist.
func SeedDefaultPlans(mongo *db.Mongo) error {
	ctx := context.Background()
	count, _ := mongo.DB.Collection("plans").CountDocuments(ctx, bson.M{})
	if count > 0 {
		return nil
	}

	now := time.Now().UTC()
	defaults := []models.Plan{
		{
			ID: "free", DisplayName: "Free", Description: "Try Topdee at no cost",
			Price: 0, Currency: "THB", IsActive: true, IsPublic: true, SortOrder: 0, ExpiryDays: 14,
			Limits: models.PlanLimits{
				Channels:         map[string]int{"facebook": 1, "line": 1},
				Members:          2,
				MessagesPerMonth: 200,
				KnowledgeBases:   1,
				StorageMB:        50,
			},
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "starter", DisplayName: "Starter", Description: "For small businesses getting started",
			Price: 590, Currency: "THB", IsActive: true, IsPublic: true, SortOrder: 1,
			Limits: models.PlanLimits{
				Channels:         map[string]int{"facebook": 1, "line": 1},
				Members:          5,
				MessagesPerMonth: 1000,
				KnowledgeBases:   3,
				StorageMB:        500,
			},
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "growth", DisplayName: "Growth", Description: "For growing teams with more volume",
			Price: 1490, Currency: "THB", IsActive: true, IsPublic: true, IsRecommended: true, SortOrder: 2,
			Limits: models.PlanLimits{
				Channels:         map[string]int{"facebook": 5, "line": 3},
				Members:          15,
				MessagesPerMonth: 5000,
				KnowledgeBases:   10,
				StorageMB:        2000,
			},
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "pro", DisplayName: "Pro", Description: "For high-volume businesses",
			Price: 2990, Currency: "THB", IsActive: true, IsPublic: true, SortOrder: 3,
			Limits: models.PlanLimits{
				Channels:         map[string]int{"facebook": 10, "line": 5},
				Members:          50,
				MessagesPerMonth: 20000,
				KnowledgeBases:   -1,
				StorageMB:        10000,
			},
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "enterprise", DisplayName: "Enterprise", Description: "Unlimited scale, dedicated support",
			Price: 0, Currency: "THB", IsActive: false, IsPublic: true, SortOrder: 4,
			Limits: models.PlanLimits{
				Channels:         map[string]int{"facebook": -1, "line": -1},
				Members:          -1,
				MessagesPerMonth: -1,
				KnowledgeBases:   -1,
				StorageMB:        -1,
			},
			CreatedAt: now, UpdatedAt: now,
		},
	}

	docs := make([]interface{}, len(defaults))
	for i, p := range defaults {
		docs[i] = p
	}
	_, err := mongo.DB.Collection("plans").InsertMany(ctx, docs)
	return err
}
