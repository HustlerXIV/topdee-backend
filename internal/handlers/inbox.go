package handlers

// Inbox API — surfaces real customer conversations (LINE, Facebook, …) from
// the `messages` collection. The dashboard playground writes its own
// messages with channel="dashboard" and is excluded from this view.
//
//   GET  /api/v1/inbox/conversations
//        → aggregated list of conversations, newest first
//   GET  /api/v1/inbox/conversations/:id/messages
//        → full transcript of one conversation, oldest first
//   POST /api/v1/inbox/conversations/:id/messages
//        → human-agent reply, dispatched to the platform's push API and
//          persisted as role="human"

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/topdee/backend/internal/channels"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type InboxHandler struct {
	mongo    *db.Mongo
	registry *channels.Registry
	store    *channels.Store
}

func NewInboxHandler(m *db.Mongo, reg *channels.Registry, store *channels.Store) *InboxHandler {
	return &InboxHandler{mongo: m, registry: reg, store: store}
}

// resolveCustomerName uses the cached profile when available, otherwise
// falls back to the placeholder ("LINE User abcd12"). One Mongo round-trip
// per conversation — cheap given the inbox cap of 200 rows.
func (h *InboxHandler) resolveCustomerName(ctx context.Context, channel, externalUserID string) string {
	if externalUserID == "" {
		return "Unknown"
	}
	if h.store != nil {
		if p, _ := h.store.GetProfile(ctx, channel, externalUserID); p != nil && p.DisplayName != "" {
			return p.DisplayName
		}
	}
	return customerNameFor(channel, externalUserID)
}

// inboxConversationView — one row in the inbox list. Computed from the
// messages collection on every request (no separate conversations table).
type inboxConversationView struct {
	ID             string    `json:"id"`
	Channel        string    `json:"channel"`
	ExternalUserID string    `json:"external_user_id"`
	CustomerName   string    `json:"customer_name"`
	Preview        string    `json:"preview"`
	LastMessageAt  time.Time `json:"last_message_at"`
	LastSenderRole string    `json:"last_sender_role"`
	MessageCount   int       `json:"message_count"`
}

// ListConversations aggregates messages by conversation_id and returns one
// row per conversation, newest first. Excludes the dashboard playground so
// the inbox doesn't drown in test conversations.
//
// We do this with a single Mongo aggregation rather than maintaining a
// separate conversations collection — fast enough at the scale a single
// tenant ever sees, and avoids a whole class of consistency bugs (what
// happens if we forget to upsert the conversation doc on a new message?).
func (h *InboxHandler) ListConversations(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)

	// Optional ?channel=line / ?channel=facebook filter.
	match := bson.M{
		"tenant_id": tid,
		"channel":   bson.M{"$ne": models.ChannelDashboard},
	}
	if ch := strings.TrimSpace(c.Query("channel")); ch != "" {
		match["channel"] = ch
	}

	pipeline := []bson.M{
		{"$match": match},
		// Sort first so $first picks up the newest message in $group.
		{"$sort": bson.M{"created_at": -1}},
		{"$group": bson.M{
			"_id":     "$conversation_id",
			"channel": bson.M{"$first": "$channel"},
			// $first scans descending — but AI replies don't carry an
			// external_user_id, so we need $max which (on strings) skips
			// empty values in favor of the populated ones. The user id is
			// also embedded in the conversation_id, so we'll fall back to
			// parsing that if both turn up empty.
			"external_user_id": bson.M{"$max": "$external_user_id"},
			"preview":          bson.M{"$first": "$content"},
			"last_message_at":  bson.M{"$first": "$created_at"},
			"last_sender_role": bson.M{"$first": "$role"},
			"message_count":    bson.M{"$sum": 1},
		}},
		{"$sort": bson.M{"last_message_at": -1}},
		{"$limit": 200},
	}

	cur, err := h.mongo.DB.Collection("messages").Aggregate(c.Context(), pipeline)
	if err != nil {
		return err
	}
	defer cur.Close(c.Context())

	type aggRow struct {
		ID             string    `bson:"_id"`
		Channel        string    `bson:"channel"`
		ExternalUserID string    `bson:"external_user_id"`
		Preview        string    `bson:"preview"`
		LastMessageAt  time.Time `bson:"last_message_at"`
		LastSenderRole string    `bson:"last_sender_role"`
		MessageCount   int       `bson:"message_count"`
	}
	var rows []aggRow
	if err := cur.All(c.Context(), &rows); err != nil {
		return err
	}

	out := make([]inboxConversationView, 0, len(rows))
	for _, r := range rows {
		uid := r.ExternalUserID
		if uid == "" {
			// Last-resort fallback: conversation IDs are always formatted
			// as "<provider>:<channel_id>:<user_id>" by the webhook router.
			if _, _, parsedUID, ok := parseConversationID(r.ID); ok {
				uid = parsedUID
			}
		}
		out = append(out, inboxConversationView{
			ID:             r.ID,
			Channel:        r.Channel,
			ExternalUserID: uid,
			CustomerName:   h.resolveCustomerName(c.Context(), r.Channel, uid),
			Preview:        truncatePreview(r.Preview, 80),
			LastMessageAt:  r.LastMessageAt,
			LastSenderRole: r.LastSenderRole,
			MessageCount:   r.MessageCount,
		})
	}
	return c.JSON(out)
}

// GetMessages returns every message in one conversation, oldest first, up
// to a sensible cap so the dashboard isn't overwhelmed by long histories.
func (h *InboxHandler) GetMessages(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	convID := c.Params("id")
	if convID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "missing conversation id")
	}

	cur, err := h.mongo.DB.Collection("messages").Find(
		c.Context(),
		bson.M{
			"tenant_id":       tid,
			"conversation_id": convID,
		},
		options.Find().
			SetSort(bson.D{{Key: "created_at", Value: 1}}).
			SetLimit(500),
	)
	if err != nil {
		return err
	}
	defer cur.Close(c.Context())

	var msgs []models.Message
	if err := cur.All(c.Context(), &msgs); err != nil {
		return err
	}
	// Empty list is preferable to null — the frontend renders the empty
	// state when there's no conversation, not when there are no messages.
	if msgs == nil {
		msgs = []models.Message{}
	}
	return c.JSON(msgs)
}

// SendMessage dispatches a manual human-agent reply through the right
// platform's push API and persists it as role="human".
//
// We rely on the conversation_id format ("<provider>:<channel_id>:<user_id>")
// to figure out which provider + connection to use — that string is the
// canonical address of a chat, set by the webhook router on every inbound
// event.
//
// `reply_token` is intentionally NOT used here: a reply token from the
// original webhook event would be long expired by the time a human types
// a reply (LINE: 30 s window). The provider's Send falls back to push
// when ReplyToken is empty.
type sendMessageReq struct {
	Text string `json:"text"`
}

func (h *InboxHandler) SendMessage(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)
	convID := c.Params("id")
	if convID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "missing conversation id")
	}

	var req sendMessageReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return fiber.NewError(fiber.StatusBadRequest, "text required")
	}

	providerName, externalChannelID, externalUserID, ok := parseConversationID(convID)
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "malformed conversation id")
	}

	provider, ok := h.registry.Get(providerName)
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "unknown provider: "+providerName)
	}

	conn, err := h.store.FindByExternal(c.Context(), providerName, externalChannelID)
	if err != nil {
		return err
	}
	if conn == nil || conn.TenantID != tid {
		return fiber.NewError(fiber.StatusNotFound, "channel connection not found for this conversation")
	}

	// Refresh credentials if applicable (LINE rotates 30-day tokens via
	// channel_id + secret). Same dance the webhook router does. Persisting
	// the refreshed token is best-effort: if the DB write fails we still
	// have a valid in-memory token and the send can proceed; we'll just
	// re-mint next time.
	if r, ok := provider.(channels.CredentialRefresher); ok {
		if refreshed, err := r.EnsureCredentials(c.Context(), conn); err != nil {
			return fiber.NewError(fiber.StatusBadGateway, "could not refresh credentials: "+err.Error())
		} else if refreshed {
			if err := h.store.UpdateCredentials(c.Context(), conn.ID, conn.Credentials); err != nil {
				log.Printf("inbox: persist refreshed credentials: %v", err)
			}
		}
	}

	evt := channels.ParsedEvent{
		ExternalChannelID: externalChannelID,
		ExternalUserID:    externalUserID,
		// ReplyToken intentionally empty — forces push API.
	}
	if err := provider.Send(c.Context(), conn, evt, text); err != nil {
		_ = h.store.MarkError(c.Context(), conn.ID, err.Error())
		return fiber.NewError(fiber.StatusBadGateway, "send failed: "+err.Error())
	}

	msg := models.Message{
		ID:             uuid.NewString(),
		TenantID:       tid,
		ConversationID: convID,
		Role:           models.RoleHuman,
		Content:        text,
		Channel:        providerName,
		ExternalUserID: externalUserID,
		Metadata: map[string]any{
			"sent_by_user_id": uid,
		},
		CreatedAt: time.Now().UTC(),
	}
	if _, err := h.mongo.DB.Collection("messages").InsertOne(c.Context(), msg); err != nil {
		return err
	}
	return c.JSON(msg)
}

// ── Helpers ────────────────────────────────────────────────────────────

// parseConversationID splits "<provider>:<channel_id>:<user_id>" — the
// canonical conversation address used by the webhook router. Returns
// ok=false if the shape doesn't match.
func parseConversationID(id string) (provider, externalChannel, externalUser string, ok bool) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// customerNameFor builds a friendly label from the platform-specific user
// id. Real names would require a profile API call to LINE / Facebook —
// we keep that as a follow-up; today's view is "LINE User abcd12".
func customerNameFor(channel, uid string) string {
	if uid == "" {
		return "Unknown"
	}
	suffix := uid
	if r := []rune(uid); len(r) > 6 {
		suffix = string(r[len(r)-6:])
	}
	switch channel {
	case models.ChannelLine:
		return "LINE User " + suffix
	case models.ChannelFacebook:
		return "FB User " + suffix
	}
	return uid
}

// truncatePreview cuts to a rune count (so Thai/CJK strings aren't sliced
// mid-codepoint). Adds an ellipsis if truncated.
func truncatePreview(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

