package handlers

// Web Widget API — public, unauthenticated endpoints used by the embeddable
// JS chat widget and by developers who want to call the API directly.
//
// Authentication: the widget_id IS the credential. It is the ExternalID of a
// ChannelConnection with provider="web". Treat it like a public API key —
// it should be hard to guess (UUID) but is expected to appear in website HTML.
//
// Endpoints:
//
//   GET  /widget/:widget_id/config   — bot name, greeting, accent colour
//   POST /widget/:widget_id/chat     — send a message, get an AI reply (sync)
//   GET  /widget/:widget_id/history  — fetch past messages for a session
//
// CORS: these endpoints must be reachable from any origin (the tenant's
// website). The main CORS middleware covers them via AllowOrigins="*".

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/models"
)

// WidgetHandler holds the dependencies for the web-widget API.
type WidgetHandler struct {
	mongo *db.Mongo
	orch  *Orchestrator
}

func NewWidgetHandler(m *db.Mongo, o *Orchestrator) *WidgetHandler {
	return &WidgetHandler{mongo: m, orch: o}
}

// resolveWidget looks up the ChannelConnection by widget_id (= ExternalID)
// and returns the tenant record alongside it. Returns 404 when not found or
// the connection is not active.
func (h *WidgetHandler) resolveWidget(c *fiber.Ctx) (*models.ChannelConnection, *models.Tenant, error) {
	wid := c.Params("widget_id")
	if wid == "" {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest, "widget_id required")
	}

	var conn models.ChannelConnection
	err := h.mongo.DB.Collection("channel_connections").
		FindOne(c.Context(), bson.M{
			"provider":    models.ProviderWeb,
			"external_id": wid,
			"status":      models.ChannelStatusActive,
		}).Decode(&conn)
	if err != nil {
		return nil, nil, fiber.NewError(fiber.StatusNotFound, "widget not found")
	}

	var tenant models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": conn.TenantID}).Decode(&tenant); err != nil {
		return nil, nil, fiber.NewError(fiber.StatusNotFound, "tenant not found")
	}
	if tenant.Suspended {
		return nil, nil, fiber.NewError(fiber.StatusForbidden, "service unavailable")
	}

	return &conn, &tenant, nil
}

// ── GET /widget/:widget_id/config ──────────────────────────────────────────
//
// Returns public metadata the widget uses on boot: bot name, greeting message,
// and accent colour. Safe to cache — changes rarely.

type widgetConfigResp struct {
	BotName         string `json:"bot_name"`
	GreetingMessage string `json:"greeting_message"`
	AccentColor     string `json:"accent_color"`
	// Locale hint — "th" | "en" | "auto". The widget defaults to browser lang.
	Locale string `json:"locale"`
}

func (h *WidgetHandler) Config(c *fiber.Ctx) error {
	conn, tenant, err := h.resolveWidget(c)
	if err != nil {
		return err
	}

	// Pull display settings from the connection's Config map (set when the
	// tenant configures the widget in the Channels page).
	cfg := conn.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	str := func(key, fallback string) string {
		if v, ok := cfg[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		return fallback
	}

	// Bot name falls back to tenant name so brand-new connections work out of
	// the box without requiring extra configuration.
	botName := str("bot_name", tenant.Name+" AI")
	greeting := str("greeting_message", "สวัสดีครับ! มีอะไรให้ช่วยไหมครับ? / Hi! How can I help you today?")
	accent := str("accent_color", "#6366f1")
	locale := str("locale", "auto")

	return c.JSON(widgetConfigResp{
		BotName:         botName,
		GreetingMessage: greeting,
		AccentColor:     accent,
		Locale:          locale,
	})
}

// ── POST /widget/:widget_id/chat ───────────────────────────────────────────
//
// Body: { "message": "...", "conversation_id": "..." (optional), "visitor_id": "..." (optional) }
// Reply: { "reply": "...", "conversation_id": "...", "sources": [...] }
//
// conversation_id is a client-side UUID the widget generates on first load and
// stores in localStorage. Sending the same id resumes the conversation thread.
// visitor_id is used as external_user_id on stored messages (for the inbox).

type widgetChatReq struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id"`
	VisitorID      string `json:"visitor_id"`
}

type widgetChatResp struct {
	Reply          string   `json:"reply"`
	ConversationID string   `json:"conversation_id"`
	Sources        []string `json:"sources,omitempty"`
}

func (h *WidgetHandler) Chat(c *fiber.Ctx) error {
	_, tenant, err := h.resolveWidget(c)
	if err != nil {
		return err
	}

	var req widgetChatReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if len([]rune(req.Message)) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "message required")
	}
	// Basic length cap — prevents abuse and keeps Gemini context sane.
	if len(req.Message) > 4000 {
		return fiber.NewError(fiber.StatusBadRequest, "message too long (max 4000 chars)")
	}

	visitorID := req.VisitorID
	if visitorID == "" {
		visitorID = "web-visitor"
	}

	reply, sources, convID, err := h.orch.HandleIncoming(
		c.Context(),
		tenant.ID,
		req.ConversationID, // empty → new conversation
		models.ChannelWeb,
		visitorID,
		req.Message,
		nil, // no attachments via web widget (yet)
	)
	if err != nil {
		return err
	}

	return c.JSON(widgetChatResp{
		Reply:          reply,
		ConversationID: convID,
		Sources:        sources,
	})
}

// ── GET /widget/:widget_id/history ─────────────────────────────────────────
//
// Query params: conversation_id (required), limit (optional, default 50, max 100)
// Returns messages oldest-first so the widget can render them in order.

type widgetHistoryMsg struct {
	Role      string    `json:"role"`    // "user" | "ai"
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *WidgetHandler) History(c *fiber.Ctx) error {
	_, tenant, err := h.resolveWidget(c)
	if err != nil {
		return err
	}

	convID := c.Query("conversation_id")
	if convID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "conversation_id required")
	}

	limit := c.QueryInt("limit", 50)
	if limit > 100 {
		limit = 100
	}

	cur, err := h.mongo.DB.Collection("messages").Find(
		c.Context(),
		bson.M{
			"tenant_id":       tenant.ID,
			"conversation_id": convID,
			"channel":         models.ChannelWeb,
			"role":            bson.M{"$in": []string{models.RoleUser, models.RoleAI}},
		},
		// We want the most-recent `limit` messages, returned oldest-first.
		// MongoDB doesn't do "last N, oldest-first" natively without a subquery,
		// so we fetch descending and reverse in Go (limit is small — max 100).
	)
	if err != nil {
		return err
	}
	defer cur.Close(c.Context())

	var raw []struct {
		Role      string    `bson:"role"`
		Content   string    `bson:"content"`
		CreatedAt time.Time `bson:"created_at"`
	}
	if err := cur.All(c.Context(), &raw); err != nil {
		return err
	}

	// Keep only the last `limit` entries (cursor has no server-side limit here).
	if len(raw) > limit {
		raw = raw[len(raw)-limit:]
	}

	msgs := make([]widgetHistoryMsg, 0, len(raw))
	for _, m := range raw {
		msgs = append(msgs, widgetHistoryMsg{
			Role:      m.Role,
			Content:   m.Content,
			CreatedAt: m.CreatedAt,
		})
	}

	return c.JSON(fiber.Map{"messages": msgs})
}
