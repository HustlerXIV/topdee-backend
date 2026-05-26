package handlers

// Channels API — list, connect, disconnect external accounts.
//
// Replaces the old single-FB / single-LINE pair. Connections live in their
// own collection now (channel_connections); a tenant can have many per
// provider, capped by their plan tier.
//
//   GET    /api/v1/channels                              list all + plan limits
//   DELETE /api/v1/channels/:id                          disconnect one
//   PUT    /api/v1/channels/line                         connect a LINE OA (manual)
//
//   POST   /api/v1/channels/facebook/oauth/start         → { login_url }
//   GET    /api/v1/channels/facebook/oauth/pages         list pages for a state
//   POST   /api/v1/channels/facebook/oauth/connect       pick pages → connections
//
//   POST   /api/v1/channels/instagram/oauth/start        → { login_url }
//   GET    /api/v1/channels/instagram/oauth/accounts     list IG accounts for a state
//   POST   /api/v1/channels/instagram/oauth/connect      pick accounts → connections

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/topdee/backend/internal/channels"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type ChannelsHandler struct {
	mongo *db.Mongo
	store *channels.Store
	cfg   *config.Config
}

func NewChannelsHandler(m *db.Mongo, cfg *config.Config) *ChannelsHandler {
	return &ChannelsHandler{
		mongo: m,
		store: channels.NewStore(m),
		cfg:   cfg,
	}
}

// connectionView is the public-safe projection of a ChannelConnection.
// Credentials are dropped (their JSON tag is `-` already, but we never lean
// on that — explicit allow-list).
//
// `webhook_url` is computed at response time using BackendPublicURL — that's
// what the customer pastes into the platform's console (LINE Developers,
// Meta, etc.).
type connectionView struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	ExternalID  string    `json:"external_id"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	WebhookURL  string    `json:"webhook_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (h *ChannelsHandler) toView(c *models.ChannelConnection) connectionView {
	return connectionView{
		ID:          c.ID,
		Provider:    c.Provider,
		ExternalID:  c.ExternalID,
		DisplayName: c.DisplayName,
		Status:      c.Status,
		Error:       c.Error,
		WebhookURL:  h.webhookURL(c.Provider, c.ExternalID),
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

// webhookURL builds the per-connection URL the customer pastes into the
// platform's console. Falls back to the catch-all `/webhooks/<provider>`
// when the external_id isn't known yet (e.g. mid-form preview).
func (h *ChannelsHandler) webhookURL(provider, externalID string) string {
	base := strings.TrimRight(h.cfg.BackendPublicURL, "/")
	if base == "" {
		base = "" // relative — frontend can prepend its own origin
	}
	if externalID == "" {
		return base + "/webhooks/" + provider
	}
	return base + "/webhooks/" + provider + "/" + externalID
}

// channelsResponse — payload of GET /channels. We bundle the per-provider
// usage / limit pairs so the UI can show "2 / 3 Facebook pages connected".
//
// `ChannelLimitMode` tells the frontend which capping strategy this plan
// uses: "per_provider" renders separate per-provider sections (legacy UI),
// "total" renders a single "Connect a channel" picker bounded by `Total`.
// `TotalUsed` is the sum across providers, surfaced so the picker can
// show "3 / 5 channels used" at a glance.
type channelsResponse struct {
	Connections      []connectionView `json:"connections"`
	Limits           map[string]int   `json:"limits"`
	Used             map[string]int   `json:"used"`
	ChannelLimitMode string           `json:"channel_limit_mode"`
	Total            int              `json:"total"`
	TotalUsed        int              `json:"total_used"`
}

// ── GET /api/v1/channels ───────────────────────────────────────────────
func (h *ChannelsHandler) List(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)

	conns, err := h.store.ListByTenant(c.Context(), tid)
	if err != nil {
		return err
	}

	// Plan lookup — we need the tenant's plan to compute limits.
	plan := "default"
	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err == nil && t.Plan != "" {
		plan = t.Plan
	}

	views := make([]connectionView, 0, len(conns))
	used := map[string]int{}
	totalUsed := 0
	for i := range conns {
		views = append(views, h.toView(&conns[i]))
		used[conns[i].Provider]++
		totalUsed++
	}

	// Load the plan policy once — covers mode, total cap, and per-provider
	// caps in a single DB roundtrip.
	policy := channels.PolicyForCtx(c.Context(), h.mongo.DB, plan)

	// Build the per-provider limits map. We always expose the per-provider
	// caps even in total mode so the frontend can use cap=0 as a "hide this
	// provider" signal regardless of which capping strategy is in effect.
	limits := map[string]int{}
	knownProviders := []string{
		models.ProviderFacebook,
		models.ProviderInstagram,
		models.ProviderLine,
		models.ProviderWeb,
	}
	for provider, cap := range policy.ProviderCaps {
		limits[provider] = cap
	}
	// For any known provider not yet in the plan document, fall back to
	// the hardcoded default so newly-added providers don't require a DB
	// migration on every existing plan.
	for _, p := range knownProviders {
		if _, ok := limits[p]; !ok {
			limits[p] = channels.LimitFor(plan, p)
		}
	}

	return c.JSON(channelsResponse{
		Connections:      views,
		Limits:           limits,
		Used:             used,
		ChannelLimitMode: policy.Mode,
		Total:            policy.Total,
		TotalUsed:        totalUsed,
	})
}

// ── DELETE /api/v1/channels/:id ────────────────────────────────────────
func (h *ChannelsHandler) Disconnect(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	id := c.Params("id")

	// Fetch before deleting so we know provider + external_id. We need these
	// to also clear the legacy tenant.line / tenant.facebook sub-document —
	// the startup migration reads those fields and would silently re-create
	// this connection on every backend restart if we leave them in place.
	conn, err := h.store.FindForTenant(c.Context(), tid, id)
	if err != nil {
		return err
	}
	if conn == nil {
		return fiber.NewError(fiber.StatusNotFound, "connection not found")
	}

	ok, err := h.store.Delete(c.Context(), tid, id)
	if err != nil {
		return err
	}
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "connection not found")
	}

	// Clear the matching legacy field from the tenant document so the
	// startup migration never re-creates this connection.
	switch conn.Provider {
	case models.ProviderLine:
		h.mongo.DB.Collection("tenants").UpdateOne(c.Context(),
			bson.M{"_id": tid, "line.channel_id": conn.ExternalID},
			bson.M{"$unset": bson.M{"line": ""}},
		)
	case models.ProviderFacebook:
		h.mongo.DB.Collection("tenants").UpdateOne(c.Context(),
			bson.M{"_id": tid, "facebook.page_id": conn.ExternalID},
			bson.M{"$unset": bson.M{"facebook": ""}},
		)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// ── PUT /api/v1/channels/line ──────────────────────────────────────────
//
// LINE connection — only Channel ID + Channel Secret. We mint an access
// token automatically via LINE's OAuth API (the same call refreshes it
// later in the webhook router), so the user never has to deal with the
// "Issue" button on the access-token row in the LINE console.
//
// On success we return the new connection plus the webhook URL the user
// should paste into LINE's "Webhook URL" field. The shape is:
//   ${BACKEND_PUBLIC_URL}/webhooks/line/${channel_id}
type lineConnectReq struct {
	ChannelID     string `json:"channel_id"`
	ChannelSecret string `json:"channel_secret"`
}

func (h *ChannelsHandler) ConnectLine(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)

	var req lineConnectReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	req.ChannelID = strings.TrimSpace(req.ChannelID)
	req.ChannelSecret = strings.TrimSpace(req.ChannelSecret)
	if req.ChannelID == "" || req.ChannelSecret == "" {
		return fiber.NewError(fiber.StatusBadRequest, "channel_id and channel_secret are required")
	}

	// Plan check.
	if err := h.enforceLimit(c, tid, models.ProviderLine, req.ChannelID); err != nil {
		return err
	}

	// Issue an access token with these credentials. If LINE rejects them,
	// the user pasted something wrong — surface a friendly error instead
	// of saving a broken connection.
	token, expiresIn, err := channels.LineIssueAccessToken(c.Context(), req.ChannelID, req.ChannelSecret)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "could not verify LINE credentials: "+err.Error())
	}

	// Best-effort: pull the display name so the dashboard shows
	// "Connected: <bot name>" instead of a numeric channel id.
	displayName := "LINE Official Account"
	if _, name, err := channels.LineBotInfo(c.Context(), token); err == nil && name != "" {
		displayName = name
	}

	creds := map[string]string{
		"channel_id":                      req.ChannelID,
		"channel_secret":                  req.ChannelSecret,
		"channel_access_token":            token,
		"channel_access_token_expires_at": time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339),
	}

	conn := &models.ChannelConnection{
		TenantID:    tid,
		Provider:    models.ProviderLine,
		ExternalID:  req.ChannelID, // path-routable: /webhooks/line/<channel_id>
		DisplayName: displayName,
		Credentials: creds,
		Status:      models.ChannelStatusActive,
		CreatedBy:   uid,
	}
	if err := h.store.Upsert(c.Context(), conn); err != nil {
		if errors.Is(err, channels.ErrConnectionTaken) {
			return fiber.NewError(fiber.StatusConflict, err.Error())
		}
		return err
	}
	return c.JSON(h.toView(conn))
}

// GET /api/v1/channels/webhook-url-template
//
// Returns the webhook URL pattern with a `{channel_id}` placeholder so the
// dashboard can render a live preview as the user types — useful before the
// connection actually exists. Cheap, idempotent, no DB hit.
func (h *ChannelsHandler) WebhookURLTemplate(c *fiber.Ctx) error {
	provider := c.Query("provider", models.ProviderLine)
	return c.JSON(fiber.Map{
		"provider": provider,
		"template": h.webhookURL(provider, "{channel_id}"),
	})
}

// ── Facebook OAuth flow ────────────────────────────────────────────────

// POST /api/v1/channels/facebook/oauth/start
//
// Generates a state token, stamps it with this tenant + user, and returns
// the Facebook Login URL. Frontend redirects the browser to that URL; Meta
// later POSTs back to /webhooks/facebook/oauth/callback (no auth) where we
// finish the dance and stash the result keyed by state.
func (h *ChannelsHandler) FacebookOAuthStart(c *fiber.Ctx) error {
	if h.cfg.FBAppID == "" || h.cfg.FBAppSecret == "" {
		return fiber.NewError(
			fiber.StatusFailedDependency,
			"Facebook Login is not configured on this server (set FB_APP_ID / FB_APP_SECRET)",
		)
	}
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)

	state := uuid.NewString()
	st := &models.FacebookOAuthState{
		State:    state,
		TenantID: tid,
		UserID:   uid,
	}
	if err := h.store.SaveOAuthState(c.Context(), st); err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"login_url": channels.FacebookLoginURL(h.cfg, state),
		"state":     state,
	})
}

// GET /api/v1/channels/facebook/oauth/pages?state=...
//
// Called by the dashboard after the OAuth callback redirected the browser
// home. Returns the list of pages the user can connect — without their
// access tokens (those stay server-side, indexed by state).
func (h *ChannelsHandler) FacebookOAuthPages(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)
	state := c.Query("state")
	if state == "" {
		return fiber.NewError(fiber.StatusBadRequest, "missing state")
	}

	st, err := h.store.GetOAuthState(c.Context(), state)
	if err != nil {
		return err
	}
	if st == nil {
		return fiber.NewError(fiber.StatusGone, "OAuth state expired")
	}
	if st.TenantID != tid || st.UserID != uid {
		return fiber.NewError(fiber.StatusForbidden, "this OAuth state belongs to another user")
	}

	type pageView struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Category string `json:"category,omitempty"`
	}
	out := make([]pageView, 0, len(st.Pages))
	for _, p := range st.Pages {
		out = append(out, pageView{ID: p.ID, Name: p.Name, Category: p.Category})
	}
	return c.JSON(fiber.Map{
		"pages": out,
		"state": state,
	})
}

// POST /api/v1/channels/facebook/oauth/connect
//
// User picked the pages in the page picker; persist them as connections.
// The state record holds the per-page access tokens we got from
// /me/accounts at callback time.
type fbConnectPagesReq struct {
	State    string   `json:"state"`
	PageIDs  []string `json:"page_ids"`
}

func (h *ChannelsHandler) FacebookOAuthConnect(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)

	var req fbConnectPagesReq
	if err := c.BodyParser(&req); err != nil || req.State == "" || len(req.PageIDs) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "state and page_ids required")
	}

	st, err := h.store.GetOAuthState(c.Context(), req.State)
	if err != nil {
		return err
	}
	if st == nil {
		return fiber.NewError(fiber.StatusGone, "OAuth state expired")
	}
	if st.TenantID != tid || st.UserID != uid {
		return fiber.NewError(fiber.StatusForbidden, "this OAuth state belongs to another user")
	}

	// Index the discovered pages by id for a quick lookup.
	byID := map[string]models.FacebookOAuthPage{}
	for _, p := range st.Pages {
		byID[p.ID] = p
	}

	// Pre-flight: make sure connecting all the requested pages won't bust
	// the plan limit. The helper handles per-provider vs. total-cap modes
	// and treats reconnects as free.
	if err := h.enforceLimitWithAdd(c, tid, models.ProviderFacebook, req.PageIDs); err != nil {
		return err
	}

	connected := []connectionView{}
	for _, pid := range req.PageIDs {
		page, ok := byID[pid]
		if !ok {
			return fiber.NewError(fiber.StatusBadRequest, "page "+pid+" not in this OAuth session")
		}
		conn := &models.ChannelConnection{
			TenantID:    tid,
			Provider:    models.ProviderFacebook,
			ExternalID:  page.ID,
			DisplayName: page.Name,
			Credentials: map[string]string{
				"page_access_token": page.AccessToken,
			},
			Config: map[string]any{
				"category": page.Category,
			},
			Status:    models.ChannelStatusActive,
			CreatedBy: uid,
		}
		if err := h.store.Upsert(c.Context(), conn); err != nil {
			if errors.Is(err, channels.ErrConnectionTaken) {
				return fiber.NewError(fiber.StatusConflict,
					"\""+page.Name+"\" is already connected to another workspace")
			}
			return err
		}
		// Subscribe the app to this page's webhooks. Best-effort — if it
		// fails (Meta rate-limit, etc.), the connection still exists and
		// the user can retry from the UI.
		if err := channels.FacebookSubscribePage(c.Context(), page.AccessToken, page.ID); err != nil {
			_ = h.store.MarkError(c.Context(), conn.ID, "subscribe failed: "+err.Error())
		}
		connected = append(connected, h.toView(conn))
	}

	// Done with the state — kill it so the same handshake can't be replayed.
	_ = h.store.DeleteOAuthState(c.Context(), req.State)

	return c.JSON(fiber.Map{"connections": connected})
}

// ── Helpers ────────────────────────────────────────────────────────────

// enforceLimit checks the tenant's plan against the channel cap. Reconnecting
// an *existing* connection (same external_id) is always allowed — we just
// upsert without counting it twice. Single-page connect → addCount = 1.
func (h *ChannelsHandler) enforceLimit(c *fiber.Ctx, tid, provider, externalID string) error {
	return h.enforceLimitWithAdd(c, tid, provider, []string{externalID})
}

// enforceLimitWithAdd is the bulk variant used by the FB / IG OAuth flows,
// where the customer picks N pages or accounts to connect in one go. It
// branches on the plan's channel-limit mode:
//
//   - "per_provider" mode (default): only the provider-specific cap applies,
//     same as the legacy behavior.
//
//   - "total" mode: the sum across all providers must stay within the plan's
//     TotalChannels. A provider with an explicit cap of 0 in PlanLimits is
//     still blocked outright (admins use that to hide a provider from a tier).
//
// In both modes, reconnecting an existing external_id owned by this tenant
// doesn't consume a new slot — we filter those out of the count.
func (h *ChannelsHandler) enforceLimitWithAdd(c *fiber.Ctx, tid, provider string, externalIDs []string) error {
	plan := tenantPlan(c.Context(), h.mongo, tid)
	policy := channels.PolicyForCtx(c.Context(), h.mongo.DB, plan)

	// Hide-by-zero: even in total mode, an admin can pin a provider to 0
	// on a given plan to gate it out.
	if policy.ProviderHidden(provider) {
		return fiber.NewError(
			fiber.StatusForbidden,
			"this plan does not include "+provider+" connections",
		)
	}

	// Count only the genuinely-new external IDs — reconnects are free.
	newAdds := 0
	for _, ext := range externalIDs {
		if ext == "" {
			// Auto-generated (e.g. web widget UUID) — always a new slot.
			newAdds++
			continue
		}
		existing, err := h.store.FindByExternal(c.Context(), provider, ext)
		if err != nil {
			return err
		}
		if existing != nil && existing.TenantID == tid {
			continue // reconnect — free
		}
		newAdds++
	}
	if newAdds == 0 {
		return nil // everything is a reconnect
	}

	if policy.IsTotalMode() {
		used, err := h.store.CountByTenant(c.Context(), tid)
		if err != nil {
			return err
		}
		if policy.Total >= 0 && int(used)+newAdds > policy.Total {
			return fiber.NewError(
				fiber.StatusForbidden,
				"this plan allows up to "+itoa(policy.Total)+" total channels (currently "+itoa(int(used))+")",
			)
		}
		return nil
	}

	// Per-provider mode — original behavior.
	limit := channels.LimitForCtx(c.Context(), h.mongo.DB, plan, provider)
	used, err := h.store.CountByProvider(c.Context(), tid, provider)
	if err != nil {
		return err
	}
	if limit >= 0 && int(used)+newAdds > limit {
		return fiber.NewError(
			fiber.StatusForbidden,
			"this plan allows up to "+itoa(limit)+" "+provider+" connections",
		)
	}
	return nil
}

func tenantPlan(ctx context.Context, m *db.Mongo, tid string) string {
	var t models.Tenant
	if err := m.DB.Collection("tenants").
		FindOne(ctx, bson.M{"_id": tid}).Decode(&t); err == nil && t.Plan != "" {
		return t.Plan
	}
	return "default"
}

func itoa(i int) string { return strconv.Itoa(i) }

// ── Instagram OAuth flow ───────────────────────────────────────────────

// POST /api/v1/channels/instagram/oauth/start
//
// Same pattern as FacebookOAuthStart — generates a state token and returns
// the Meta Login URL (with Instagram-specific scopes). The browser is
// redirected to Meta; after the user authorizes, Meta calls our callback at
// /webhooks/instagram/oauth/callback, which discovers the user's Instagram
// Business Accounts and bounces the browser back here.
func (h *ChannelsHandler) InstagramOAuthStart(c *fiber.Ctx) error {
	if h.cfg.FBAppID == "" || h.cfg.FBAppSecret == "" {
		return fiber.NewError(
			fiber.StatusFailedDependency,
			"Instagram Login is not configured on this server (set FB_APP_ID / FB_APP_SECRET)",
		)
	}
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)

	state := newUUID()
	st := &models.InstagramOAuthState{
		State:    state,
		TenantID: tid,
		UserID:   uid,
	}
	if err := h.store.SaveIGOAuthState(c.Context(), st); err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"login_url": channels.InstagramLoginURL(h.cfg, state),
		"state":     state,
	})
}

// GET /api/v1/channels/instagram/oauth/accounts?state=...
//
// Returns the list of Instagram Business Accounts the user can connect —
// without their access tokens (those stay server-side, indexed by state).
func (h *ChannelsHandler) InstagramOAuthAccounts(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)
	state := c.Query("state")
	if state == "" {
		return fiber.NewError(fiber.StatusBadRequest, "missing state")
	}

	st, err := h.store.GetIGOAuthState(c.Context(), state)
	if err != nil {
		return err
	}
	if st == nil {
		return fiber.NewError(fiber.StatusGone, "OAuth state expired")
	}
	if st.TenantID != tid || st.UserID != uid {
		return fiber.NewError(fiber.StatusForbidden, "this OAuth state belongs to another user")
	}

	type accountView struct {
		IGID     string `json:"igid"`
		Name     string `json:"name"`
		Username string `json:"username,omitempty"`
	}
	out := make([]accountView, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		out = append(out, accountView{IGID: a.IGID, Name: a.Name, Username: a.Username})
	}
	return c.JSON(fiber.Map{
		"accounts": out,
		"state":    state,
	})
}

// POST /api/v1/channels/instagram/oauth/connect
//
// User picked which Instagram Business Accounts to connect; persist them.
type igConnectReq struct {
	State   string   `json:"state"`
	IGIDs   []string `json:"ig_ids"`
}

func (h *ChannelsHandler) InstagramOAuthConnect(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)

	var req igConnectReq
	if err := c.BodyParser(&req); err != nil || req.State == "" || len(req.IGIDs) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "state and ig_ids required")
	}

	st, err := h.store.GetIGOAuthState(c.Context(), req.State)
	if err != nil {
		return err
	}
	if st == nil {
		return fiber.NewError(fiber.StatusGone, "OAuth state expired")
	}
	if st.TenantID != tid || st.UserID != uid {
		return fiber.NewError(fiber.StatusForbidden, "this OAuth state belongs to another user")
	}

	// Index discovered accounts by IGID for quick lookup.
	byID := map[string]models.InstagramOAuthAccount{}
	for _, a := range st.Accounts {
		byID[a.IGID] = a
	}

	// Pre-flight plan limit check — branches on per-provider vs total mode.
	if err := h.enforceLimitWithAdd(c, tid, models.ProviderInstagram, req.IGIDs); err != nil {
		return err
	}

	connected := []connectionView{}
	for _, igid := range req.IGIDs {
		acct, ok := byID[igid]
		if !ok {
			return fiber.NewError(fiber.StatusBadRequest, "account "+igid+" not in this OAuth session")
		}
		displayName := acct.Name
		if acct.Username != "" {
			displayName = "@" + acct.Username
		}
		conn := &models.ChannelConnection{
			TenantID:    tid,
			Provider:    models.ProviderInstagram,
			ExternalID:  acct.IGID,
			DisplayName: displayName,
			Credentials: map[string]string{
				"page_access_token": acct.PageAccessToken,
				"ig_user_id":        acct.IGID,
				"page_id":           acct.PageID,
			},
			Status:    models.ChannelStatusActive,
			CreatedBy: uid,
		}
		if err := h.store.Upsert(c.Context(), conn); err != nil {
			if errors.Is(err, channels.ErrConnectionTaken) {
				return fiber.NewError(fiber.StatusConflict,
					"\""+displayName+"\" is already connected to another workspace")
			}
			return err
		}
		// Subscribe to Instagram DM webhooks. Best-effort.
		if err := channels.InstagramSubscribeAccount(c.Context(), acct.PageAccessToken, acct.IGID); err != nil {
			_ = h.store.MarkError(c.Context(), conn.ID, "subscribe failed: "+err.Error())
		}
		connected = append(connected, h.toView(conn))
	}

	_ = h.store.DeleteIGOAuthState(c.Context(), req.State)
	return c.JSON(fiber.Map{"connections": connected})
}

// newUUID generates a random UUID string. Wraps uuid.NewString so callers
// don't need to import the uuid package.
func newUUID() string {
	return uuid.NewString()
}

// ── POST /api/v1/channels/web ──────────────────────────────────────────────
//
// Creates a Web Widget connection. A UUID is auto-generated as the widget_id
// (ExternalID). The tenant pastes this into the JS embed snippet on their site.
//
// Body (optional):
//
//	{
//	  "display_name":     "My Website Chat",
//	  "bot_name":         "Aria",
//	  "greeting_message": "Hi! How can I help?",
//	  "accent_color":     "#6366f1"
//	}
type webConnectReq struct {
	DisplayName     string `json:"display_name"`
	BotName         string `json:"bot_name"`
	GreetingMessage string `json:"greeting_message"`
	AccentColor     string `json:"accent_color"`
}

type webConnectResp struct {
	Connection connectionView `json:"connection"`
	// WidgetID is the public identifier to embed in the script tag.
	WidgetID string `json:"widget_id"`
	// EmbedCode is a ready-to-copy <script> tag.
	EmbedCode string `json:"embed_code"`
}

func (h *ChannelsHandler) ConnectWeb(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	uid := middleware.UserID(c)

	var req webConnectReq
	_ = c.BodyParser(&req) // body is optional

	if err := h.enforceLimit(c, tid, models.ProviderWeb, ""); err != nil {
		return err
	}

	widgetID := uuid.NewString()
	displayName := req.DisplayName
	if displayName == "" {
		displayName = "Website Chat"
	}

	config := map[string]any{}
	if req.BotName != "" {
		config["bot_name"] = req.BotName
	}
	if req.GreetingMessage != "" {
		config["greeting_message"] = req.GreetingMessage
	}
	if req.AccentColor != "" {
		config["accent_color"] = req.AccentColor
	}

	conn := &models.ChannelConnection{
		TenantID:    tid,
		Provider:    models.ProviderWeb,
		ExternalID:  widgetID,
		DisplayName: displayName,
		Credentials: map[string]string{},
		Config:      config,
		Status:      models.ChannelStatusActive,
		CreatedBy:   uid,
	}
	if err := h.store.Upsert(c.Context(), conn); err != nil {
		return err
	}

	embedCode := `<script src="` + strings.TrimRight(h.cfg.BackendPublicURL, "/") + `/widget.js" data-widget-id="` + widgetID + `"></script>`

	return c.Status(fiber.StatusCreated).JSON(webConnectResp{
		Connection: h.toView(conn),
		WidgetID:   widgetID,
		EmbedCode:  embedCode,
	})
}
