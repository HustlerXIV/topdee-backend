package handlers

// Repurposed in the Shape 2 refactor: this file now hosts the playground
// (in-dashboard test chat) and the channel-message ingress. Both run through
// the same orchestrator that loads the platform agent + tenant KBs and calls
// the AI service.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/topdee/backend/internal/clients"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

// sourceMentionPatterns — last line of defense against the AI leaking
// internal filenames to real customers. We instruct the model not to
// cite sources in the prompt, and we strip filename labels from the RAG
// context, but Gemini still occasionally produces "(source: foo.pdf)"
// anyway. So on every non-dashboard channel we run the reply through
// these regexes before storing or sending.
//
// Patterns we strip:
//
//   - (source: foo.pdf)         — round brackets
//   - [SRC: bar.pdf]            — square brackets, abbreviation
//   - {sources: a, b}           — curly brackets, plural
//   - (ที่มา: คู่มือ.pdf)            — Thai
//   - (แหล่งที่มา: ...)            — Thai (alt)
//   - (อ้างอิง: ...)              — Thai (citation)
//
// The leading `\s*` is intentional — we eat the whitespace right before
// the parenthesized group too, so "Hello (source: x). World" becomes
// "Hello. World" instead of "Hello . World".
var sourceMentionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\s*[\(\[\{]\s*(?:sources?|src|ที่มา|แหล่งที่มา|อ้างอิง)\s*[:：][^\)\]\}]*[\)\]\}]`),
	// Trailing "Sources:" section the model sometimes appends:
	//
	//   Sources:
	//   - Japan trip.pdf
	//   - handbook.pdf
	//
	// (?m) lets ^ match start-of-line; we're greedy across the bullet
	// lines that follow.
	regexp.MustCompile(`(?im)^\s*(?:sources?|references?|citations?|ที่มา|แหล่งที่มา|อ้างอิง)\s*[:：]\s*\n(?:[-*•]\s*.*\n?)+`),
}

// stripSourceMentions removes any (source: ...) and trailing "Sources:"
// blocks from a reply. Returns the cleaned text with leading/trailing
// whitespace trimmed so we don't accidentally emit a message that's just
// a newline.
func stripSourceMentions(s string) string {
	for _, re := range sourceMentionPatterns {
		s = re.ReplaceAllString(s, "")
	}
	// Collapse runs of 3+ newlines that the strip might leave behind.
	s = regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

type Orchestrator struct {
	mongo *db.Mongo
	ai    *clients.AIClient
	cfg   *config.Config
}

type runtimeBotSettings struct {
	systemPrompt string
	model        string
	temperature  float64
	mode         string
}

func NewOrchestrator(m *db.Mongo, ai *clients.AIClient, cfg *config.Config) *Orchestrator {
	return &Orchestrator{mongo: m, ai: ai, cfg: cfg}
}

// HandleIncoming runs the platform-agent pipeline for a single inbound message
// and returns the AI reply text + sources. It always persists the inbound
// turn, then follows the tenant reply mode before generating or sending an AI
// turn. Used by both the playground handler and channel webhooks.
func (o *Orchestrator) HandleIncoming(
	ctx context.Context,
	tenantID, conversationID, channel, externalUserID, message string,
	attachments []models.Attachment,
) (reply string, sources []string, convID string, err error) {
	if conversationID == "" {
		conversationID = uuid.NewString()
	}

	// Refuse if the tenant has been suspended by a platform admin. This is
	// the primary chokepoint — every channel (playground, FB, LINE) flows
	// through HandleIncoming, so one check covers them all.
	var tenant models.Tenant
	if err := o.mongo.DB.Collection("tenants").
		FindOne(ctx, bson.M{"_id": tenantID}).Decode(&tenant); err == nil {
		if tenant.Suspended {
			return "", nil, conversationID, errors.New("tenant suspended")
		}
	}

	history, err := o.loadHistory(ctx, tenantID, conversationID, 20)
	if err != nil {
		return "", nil, conversationID, err
	}

	kbIDs, err := o.tenantKnowledgeBaseIDs(ctx, tenantID)
	if err != nil {
		return "", nil, conversationID, err
	}

	// Pull the per-tenant bot settings (one tiny lookup). Empty fields fall
	// back to env defaults inside resolveBotSettings.
	bot := o.resolveBotSettings(ctx, tenantID)

	now := time.Now().UTC()
	content := strings.TrimSpace(message)
	if content == "" && len(attachments) > 0 {
		content = "[Image]"
	}
	userMsg := models.Message{
		ID:             uuid.NewString(),
		TenantID:       tenantID,
		ConversationID: conversationID,
		Role:           models.RoleUser,
		Content:        content,
		Channel:        channel,
		ExternalUserID: externalUserID,
		Attachments:    attachments,
		CreatedAt:      now,
	}
	if _, err := o.mongo.DB.Collection("messages").InsertOne(ctx, userMsg); err != nil {
		return "", nil, conversationID, err
	}

	if bot.mode == "manual" || strings.TrimSpace(message) == "" {
		return "", nil, conversationID, nil
	}

	// Source citations are dashboard-only. Real customers on LINE / Facebook
	// / etc. should never see internal filenames in the reply text — and
	// the AI service won't include them as a separate field either when
	// this flag is false.
	mentionSources := channel == models.ChannelDashboard

	resp, err := o.ai.Chat(ctx, clients.ChatRequest{
		TenantID:         tenantID,
		ConversationID:   conversationID,
		SystemPrompt:     bot.systemPrompt,
		Model:            bot.model,
		Temperature:      bot.temperature,
		History:          history,
		Message:          message,
		KnowledgeBaseIDs: kbIDs,
		MentionSources:   mentionSources,
	})
	if err != nil {
		return "", nil, conversationID, err
	}

	// Belt-and-suspenders: even if Gemini ignored the prompt instruction,
	// strip parenthesized source mentions and trailing "Sources:" blocks
	// before this reply leaves our system on a customer-facing channel.
	// The dashboard playground keeps citations so staff can verify which
	// document each answer is grounded in.
	if !mentionSources {
		resp.Reply = stripSourceMentions(resp.Reply)
		// Empty `sources` too — defensive: the AI service already does
		// this when mention_sources=false, but if a future caller forgets
		// to set the flag we don't want to expose them by accident.
		resp.Sources = nil
	}

	role := models.RoleAI
	if channel != models.ChannelDashboard && bot.mode == "suggest" {
		role = models.RoleSuggestion
	}

	aiMsg := models.Message{
		ID:             uuid.NewString(),
		TenantID:       tenantID,
		ConversationID: conversationID,
		Role:           role,
		Content:        resp.Reply,
		Channel:        channel,
		Metadata: map[string]any{
			"sources":     resp.Sources,
			"tokens_used": resp.TokensUsed,
		},
		CreatedAt: time.Now().UTC(),
	}
	if _, err := o.mongo.DB.Collection("messages").InsertOne(ctx, aiMsg); err != nil {
		return "", nil, conversationID, err
	}

	if role == models.RoleSuggestion {
		return "", resp.Sources, conversationID, nil
	}
	return resp.Reply, resp.Sources, conversationID, nil
}

func (o *Orchestrator) loadHistory(ctx context.Context, tenantID, convID string, limit int) ([]clients.ChatMessage, error) {
	cur, err := o.mongo.DB.Collection("messages").Find(
		ctx,
		bson.M{"tenant_id": tenantID, "conversation_id": convID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(int64(limit)),
	)
	if err != nil {
		return nil, err
	}
	var msgs []models.Message
	if err := cur.All(ctx, &msgs); err != nil {
		return nil, err
	}
	out := make([]clients.ChatMessage, 0, len(msgs))
	// reverse to chronological
	for i := len(msgs) - 1; i >= 0; i-- {
		role := msgs[i].Role
		if role == models.RoleSuggestion {
			continue
		}
		if role == models.RoleAI || role == models.RoleHuman {
			role = "assistant"
		}
		out = append(out, clients.ChatMessage{Role: role, Content: msgs[i].Content})
	}
	return out, nil
}

// resolveBotSettings returns (systemPrompt, model, temperature) for a tenant,
// blending the saved BotSettings with env defaults.
//
// Layout of the final system prompt sent to the AI:
//
//	<Identity block>           ← name + persona + language, prepended
//	<User-provided prompt>     ← what the admin wrote on /bot
//
// We put identity FIRST (highest weight in most LLMs) and phrase it
// unambiguously so questions like "what's your name?" don't fall through
// to the RAG "say you don't know" rule. The user prompt that follows can
// freely talk about products, policies, etc — those go through the RAG
// pipeline as before.
//
// On any Mongo error we silently fall back to env defaults — chat must never
// fail just because the bot-settings lookup hiccuped.
func (o *Orchestrator) resolveBotSettings(ctx context.Context, tenantID string) runtimeBotSettings {
	settings := runtimeBotSettings{
		systemPrompt: o.cfg.PlatformSystemPrompt,
		model:        o.cfg.PlatformModel,
		temperature:  o.cfg.PlatformTemperature,
		mode:         "auto",
	}

	var t models.Tenant
	if err := o.mongo.DB.Collection("tenants").
		FindOne(ctx, bson.M{"_id": tenantID}).Decode(&t); err != nil {
		return settings
	}
	b := t.Bot
	if b == nil {
		return settings
	}

	if b.SystemPrompt != "" {
		settings.systemPrompt = b.SystemPrompt
	}
	if b.Model != "" {
		settings.model = b.Model
	}
	if b.Temperature != nil {
		settings.temperature = *b.Temperature
	}
	if b.Mode == "auto" || b.Mode == "suggest" || b.Mode == "manual" {
		settings.mode = b.Mode
	}

	// Build the identity preamble. Each line is a directive with no ambiguity.
	identity := []string{}
	if b.Name != "" {
		identity = append(identity,
			"Your name is \""+b.Name+"\". This is who you are — when a user "+
				"asks your name (e.g. \"what's your name?\", \"คุณชื่ออะไร\", "+
				"\"who are you?\"), answer with this name. Do not say you have no name.",
		)
	}
	if hint := personaHint(b.Persona); hint != "" {
		identity = append(identity, hint)
	}
	if hint := languageHint(b.Language); hint != "" {
		identity = append(identity, hint)
	}

	// Tack on a live business-hours status line so the AI knows whether
	// the shop is open right now.
	if hint := businessHoursHint(t.BusinessHours); hint != "" {
		identity = append(identity, hint)
	}

	if len(identity) > 0 {
		preamble := "[Identity]\n" + strings.Join(identity, "\n") + "\n\n[Instructions]\n"
		settings.systemPrompt = preamble + settings.systemPrompt
	}

	return settings
}

// businessHoursHint returns a multi-line directive describing the open/closed
// status AND telling the AI exactly how to behave during after-hours. We
// compute this at request time so it always reflects "right now" rather than
// whenever the schedule was last saved.
//
// Behavior the prompt encodes:
//
//   - OPEN  → carry on as normal.
//   - CLOSED → still answer the customer's question using the knowledge base,
//     then append a bilingual closing-time notice. Only fall back to
//     the saved out-of-hours message when the question genuinely
//     needs a human (refund, complaint, custom request).
//
// Returns empty string when the tenant hasn't configured hours yet.
func businessHoursHint(bh *models.BusinessHours) string {
	if bh == nil {
		return ""
	}
	loc, err := time.LoadLocation(bh.Timezone)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	day := bh.Days[int(now.Weekday())]

	// ── OPEN right now ───────────────────────────────────────────────
	currentHM := now.Format("15:04")
	if day.Enabled && currentHM >= day.Open && currentHM < day.Close {
		return "Business status: OPEN now (until " + day.Close + " " + bh.Timezone + "). " +
			"Answer the customer normally."
	}

	// ── CLOSED — work out when we're back open ───────────────────────
	var nextLabel, nextLabelTh, nextTime string

	// Same-day before-open is the most informative case: we're back in a
	// few hours, not tomorrow.
	if day.Enabled && currentHM < day.Open {
		nextLabel = "today"
		nextLabelTh = "วันนี้"
		nextTime = day.Open
	} else {
		// Either today is closed entirely, or we're past close — scan forward.
		nextLabel, nextTime = nextOpen(bh, now)
		nextLabelTh = thaiDayLabel(nextLabel)
	}

	// Default fallback line (shown if no specific opening time is known).
	thBack := "เร็วที่สุด"
	enBack := "as soon as possible"
	if nextTime != "" {
		thBack = nextLabelTh + " " + nextTime + " น."
		enBack = nextLabel + " at " + nextTime + " (" + bh.Timezone + ")"
	}

	thNotice := "ขณะนี้อยู่นอกเวลาทำการ ทีมงานจะกลับมาให้บริการ " + thBack + " ค่ะ 🙏"
	enNotice := "We're currently outside business hours. Our team will be back " + enBack + "."

	out := "Business status: CLOSED. Next opening: " + nextLabel + " at " + nextTime + " (" + bh.Timezone + ").\n" +
		"After-hours behavior:\n" +
		"  1. ALWAYS try to answer the customer's question using the provided knowledge — " +
		"do NOT refuse just because we're closed.\n" +
		"  2. At the END of your reply, append the appropriate notice in the customer's language:\n" +
		"     • Thai customer:   \"" + thNotice + "\"\n" +
		"     • English customer: \"" + enNotice + "\"\n" +
		"  3. ONLY if the question genuinely requires a human (refund, complaint, custom order, " +
		"personal account changes), reply primarily with: \"" + bh.OutOfHoursMessage + "\""
	return out
}

// thaiDayLabel maps a short English weekday label (or "today"/"tomorrow")
// to its Thai equivalent — used in the after-hours notice.
func thaiDayLabel(en string) string {
	switch en {
	case "today":
		return "วันนี้"
	case "tomorrow", "tomorrow (Sun)", "tomorrow (Mon)", "tomorrow (Tue)",
		"tomorrow (Wed)", "tomorrow (Thu)", "tomorrow (Fri)", "tomorrow (Sat)":
		return "พรุ่งนี้"
	case "Sun":
		return "วันอาทิตย์"
	case "Mon":
		return "วันจันทร์"
	case "Tue":
		return "วันอังคาร"
	case "Wed":
		return "วันพุธ"
	case "Thu":
		return "วันพฤหัสบดี"
	case "Fri":
		return "วันศุกร์"
	case "Sat":
		return "วันเสาร์"
	default:
		return en
	}
}

// nextOpen scans forward up to 7 days for the next enabled day; returns the
// human-readable label (e.g. "Mon", "tomorrow") and the open time string.
// Returns ("", "") if every day in the schedule is disabled.
func nextOpen(bh *models.BusinessHours, from time.Time) (string, string) {
	weekdayName := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	for offset := 1; offset <= 7; offset++ {
		idx := (int(from.Weekday()) + offset) % 7
		d := bh.Days[idx]
		if !d.Enabled {
			continue
		}
		label := weekdayName[idx]
		if offset == 1 {
			label = "tomorrow (" + label + ")"
		}
		return label, d.Open
	}
	return "", ""
}

func personaHint(p string) string {
	switch p {
	case "friendly":
		return "Tone: warm, conversational, polite. Light emoji is fine."
	case "formal":
		return "Tone: professional and concise. Avoid slang and emoji."
	case "fun":
		return "Tone: playful and energetic. Use emoji generously."
	case "concise":
		return "Tone: short and direct. One or two sentences when possible."
	default:
		return ""
	}
}

func languageHint(lang string) string {
	switch lang {
	case "th":
		return "Always reply in Thai unless the customer writes in another language."
	case "en":
		return "Always reply in English unless the customer writes in another language."
	case "mix":
		return "Reply in whichever language the customer used (Thai or English)."
	default:
		return ""
	}
}

func (o *Orchestrator) tenantKnowledgeBaseIDs(ctx context.Context, tenantID string) ([]string, error) {
	cur, err := o.mongo.DB.Collection("knowledge_bases").Find(
		ctx,
		bson.M{"tenant_id": tenantID},
		options.Find().SetProjection(bson.M{"_id": 1}),
	)
	if err != nil {
		return nil, err
	}
	type idDoc struct {
		ID string `bson:"_id"`
	}
	var docs []idDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	return ids, nil
}

// ---- HTTP handler: the dashboard playground -----------------------------

type PlaygroundHandler struct {
	o     *Orchestrator
	mongo *db.Mongo
}

func NewPlaygroundHandler(o *Orchestrator, m *db.Mongo) *PlaygroundHandler {
	return &PlaygroundHandler{o: o, mongo: m}
}

type playgroundReq struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
}

func (h *PlaygroundHandler) Send(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	var req playgroundReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if req.Message == "" {
		return fiber.NewError(fiber.StatusBadRequest, "message required")
	}

	reply, sources, convID, err := h.o.HandleIncoming(
		c.Context(), tid, req.ConversationID, models.ChannelDashboard, "", req.Message, nil,
	)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return c.JSON(fiber.Map{
		"conversation_id": convID,
		"reply":           reply,
		"sources":         sources,
	})
}

func (h *PlaygroundHandler) GetConversation(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	convID := c.Params("id")
	cur, err := h.mongo.DB.Collection("messages").Find(
		c.Context(),
		bson.M{"tenant_id": tid, "conversation_id": convID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}),
	)
	if err != nil {
		return err
	}
	var out []models.Message
	if err := cur.All(c.Context(), &out); err != nil {
		return err
	}
	if out == nil {
		out = []models.Message{}
	}
	return c.JSON(out)
}

// playgroundConversationSummary is the row shape returned by ListConversations.
// Mongo aggregation will produce these via a $group stage.
type playgroundConversationSummary struct {
	ID             string    `bson:"_id" json:"id"`
	FirstMessageAt time.Time `bson:"first_message_at" json:"first_message_at"`
	LastMessageAt  time.Time `bson:"last_message_at" json:"last_message_at"`
	Preview        string    `bson:"preview" json:"preview"`
	MessageCount   int       `bson:"message_count" json:"message_count"`
}

// ListConversations returns the playground (channel=dashboard) conversations
// for the current tenant, most-recently-active first. The dashboard uses this
// to render a "past tests" picker — admins can re-open a previous test session
// instead of always starting fresh.
//
// Capped at 30 rows; older sessions stay in Mongo and can still be opened
// directly by id via GetConversation.
func (h *PlaygroundHandler) ListConversations(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	pipeline := []bson.M{
		{"$match": bson.M{
			"tenant_id": tid,
			"channel":   models.ChannelDashboard,
		}},
		// Sort first so $first / $last in the group stage hit the right ends.
		{"$sort": bson.M{"created_at": 1}},
		{"$group": bson.M{
			"_id":              "$conversation_id",
			"first_message_at": bson.M{"$first": "$created_at"},
			"last_message_at":  bson.M{"$last": "$created_at"},
			"preview": bson.M{
				"$first": bson.M{
					"$cond": []any{
						bson.M{"$eq": []any{"$role", models.RoleUser}},
						"$content",
						"",
					},
				},
			},
			"message_count": bson.M{"$sum": 1},
		}},
		{"$sort": bson.M{"last_message_at": -1}},
		{"$limit": 30},
	}

	cur, err := h.mongo.DB.Collection("messages").Aggregate(c.Context(), pipeline)
	if err != nil {
		return err
	}
	var out []playgroundConversationSummary
	if err := cur.All(c.Context(), &out); err != nil {
		return err
	}
	if out == nil {
		out = []playgroundConversationSummary{}
	}
	return c.JSON(out)
}
