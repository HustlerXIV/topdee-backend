package handlers

// Bot settings — per-tenant override of the platform agent (name, persona,
// language, mode, system prompt, model, temperature). Stored as a `bot`
// sub-document on the tenant. Returns env defaults when the tenant hasn't
// configured anything yet.

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type BotHandler struct {
	mongo *db.Mongo
	cfg   *config.Config
}

func NewBotHandler(m *db.Mongo, cfg *config.Config) *BotHandler {
	return &BotHandler{mongo: m, cfg: cfg}
}

// ── Allow-lists ────────────────────────────────────────────────────────
// Reject anything off-list to keep the UI selects aligned with the API.

var (
	allowedLanguages = map[string]bool{"th": true, "en": true, "mix": true, "auto": true}
	allowedPersonas  = map[string]bool{
		"friendly": true, "formal": true, "fun": true, "concise": true,
	}
	allowedModes = map[string]bool{
		"auto": true, "suggest": true, "manual": true,
	}
)

// ── GET /api/v1/bot ────────────────────────────────────────────────────
//
// Always returns a fully-populated response, even for tenants that haven't
// saved anything yet — the UI can render fields without null-checks.
func (h *BotHandler) Get(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}

	out := h.merge(t.Bot)
	return c.JSON(out)
}

// ── PUT /api/v1/bot ────────────────────────────────────────────────────
//
// Body shape mirrors BotSettings minus UpdatedAt. Empty optional fields
// (model="", temperature=nil) are stored as zero values, which the runtime
// then interprets as "fall back to env default".
type botUpdateReq struct {
	Name         string   `json:"name"`
	Language     string   `json:"language"`
	Persona      string   `json:"persona"`
	Mode         string   `json:"mode"`
	SystemPrompt string   `json:"system_prompt"`
	Model        string   `json:"model"`
	Temperature  *float64 `json:"temperature"`
}

func (h *BotHandler) Update(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	var req botUpdateReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}

	// Light validation — accept blank to mean "use default", but if the user
	// did pick something it has to be on-list.
	if req.Language != "" && !allowedLanguages[req.Language] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid language")
	}
	if req.Persona != "" && !allowedPersonas[req.Persona] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid persona")
	}
	if req.Mode != "" && !allowedModes[req.Mode] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid mode")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return fiber.NewError(fiber.StatusBadRequest, "temperature must be between 0 and 2")
	}
	if len(req.Name) > 80 {
		return fiber.NewError(fiber.StatusBadRequest, "name too long")
	}
	if len(req.SystemPrompt) > 8000 {
		return fiber.NewError(fiber.StatusBadRequest, "system_prompt too long")
	}

	settings := models.BotSettings{
		Name:         strings.TrimSpace(req.Name),
		Language:     req.Language,
		Persona:      req.Persona,
		Mode:         req.Mode,
		SystemPrompt: req.SystemPrompt,
		Model:        strings.TrimSpace(req.Model),
		Temperature:  req.Temperature,
		UpdatedAt:    time.Now().UTC(),
	}

	_, err := h.mongo.DB.Collection("tenants").UpdateOne(
		c.Context(),
		bson.M{"_id": tid},
		bson.M{"$set": bson.M{"bot": settings}},
	)
	if err != nil {
		return err
	}
	return c.JSON(h.merge(&settings))
}

// merge returns a fully-populated view: tenant overrides where set, env
// defaults everywhere else. Used by Get and as the response body of Update.
func (h *BotHandler) merge(stored *models.BotSettings) models.BotSettings {
	out := models.BotSettings{
		Name:         "AI Assistant",
		Language:     "th",
		Persona:      "friendly",
		Mode:         "auto",
		SystemPrompt: h.cfg.PlatformSystemPrompt,
		Model:        h.cfg.PlatformModel,
		Temperature:  ptrFloat(h.cfg.PlatformTemperature),
	}
	if stored == nil {
		return out
	}
	if stored.Name != "" {
		out.Name = stored.Name
	}
	if stored.Language != "" {
		out.Language = stored.Language
	}
	if stored.Persona != "" {
		out.Persona = stored.Persona
	}
	if stored.Mode != "" {
		out.Mode = stored.Mode
	}
	if stored.SystemPrompt != "" {
		out.SystemPrompt = stored.SystemPrompt
	}
	if stored.Model != "" {
		out.Model = stored.Model
	}
	if stored.Temperature != nil {
		out.Temperature = stored.Temperature
	}
	out.UpdatedAt = stored.UpdatedAt
	return out
}

func ptrFloat(v float64) *float64 { return &v }
