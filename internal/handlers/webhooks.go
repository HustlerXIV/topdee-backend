package handlers

// Generic webhook router. One handler powers every social-platform webhook
// — adding a new provider doesn't touch this file. The flow is:
//
//   1. /webhooks/:provider routes to the registry → bail with 404 if unknown.
//   2. GET requests trigger the provider's optional handshake (FB, etc.).
//   3. POST requests:
//      a. Provider.ParseEvents → list of inbound events.
//      b. For each event we look up the connection by (provider, external_id).
//      c. Provider.VerifySignature with that connection's secrets.
//      d. Orchestrator.HandleIncoming runs the AI turn.
//      e. Provider.Send pushes the reply back.
//
// Stripe still has its own dedicated handler (`webhook_stripe.go`) because
// it isn't a chat platform and doesn't fit the Provider interface.

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/topdee/backend/internal/channels"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/models"
	"github.com/topdee/backend/internal/realtime"
)

// WebhookHandler returns the generic GET/POST handler for /webhooks/:provider.
//
// Both verbs share the same Fiber handler — we dispatch on c.Method() so the
// router only needs one entry per provider.
func WebhookHandler(reg *channels.Registry, store *channels.Store, m *db.Mongo, o *Orchestrator, cfg *config.Config, hub *realtime.Hub) fiber.Handler {
	return func(c *fiber.Ctx) error {
		name := strings.ToLower(c.Params("provider"))
		p, ok := reg.Get(name)
		if !ok {
			return fiber.NewError(fiber.StatusNotFound, "unknown provider: "+name)
		}

		// ── GET handshake (Facebook subscription verification) ─────────
		if c.Method() == fiber.MethodGet {
			if ok, body := p.HandshakeVerify(c.Queries(), cfg); ok {
				return c.SendString(body)
			}
			return fiber.NewError(fiber.StatusForbidden, "verify failed")
		}

		// ── POST event ─────────────────────────────────────────────────
		body := c.Body()
		headers := map[string]string{}
		c.Request().Header.VisitAll(func(k, v []byte) {
			headers[string(k)] = string(v)
		})

		events, err := p.ParseEvents(body)
		if err != nil {
			// Malformed payload: ack with 200 so the platform doesn't retry
			// us forever, but log for visibility.
			log.Printf("webhook %s: parse error: %v", name, err)
			return c.SendStatus(fiber.StatusOK)
		}

		// Per-tenant URL form: /webhooks/<provider>/<external_id>. When
		// present, override the events' channel id with the path param
		// — saves us from having to parse it out of the body and lets the
		// customer paste a unique URL into their LINE / future-platform
		// console.
		if extID := c.Params("external_id"); extID != "" {
			for i := range events {
				events[i].ExternalChannelID = extID
			}
		}

		// Process asynchronously so we always 200 fast — most platforms
		// time out the webhook after a few seconds.
		go processEvents(p, store, m, o, cfg, hub, headers, body, events)
		return c.SendStatus(fiber.StatusOK)
	}
}

// processEvents groups events by external channel id, looks up each
// connection, verifies signature, runs orchestrator + send. One slow page
// can't block another.
func processEvents(
	p channels.Provider,
	store *channels.Store,
	m *db.Mongo,
	o *Orchestrator,
	cfg *config.Config,
	hub *realtime.Hub,
	headers map[string]string,
	body []byte,
	events []channels.ParsedEvent,
) {
	// Group so we only do one signature check per (channel, request).
	byChannel := map[string][]channels.ParsedEvent{}
	for _, e := range events {
		byChannel[e.ExternalChannelID] = append(byChannel[e.ExternalChannelID], e)
	}

	for externalID, batch := range byChannel {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		conn, err := store.FindByExternal(ctx, p.Name(), externalID)
		if err != nil {
			log.Printf("webhook %s: connection lookup: %v", p.Name(), err)
			cancel()
			continue
		}
		if conn == nil {
			log.Printf("webhook %s: no connection for external_id=%s", p.Name(), externalID)
			cancel()
			continue
		}
		if !p.VerifySignature(headers, body, cfg, conn) {
			log.Printf("webhook %s: invalid signature for external_id=%s", p.Name(), externalID)
			cancel()
			continue
		}
		if conn.Status == models.ChannelStatusDisabled {
			cancel()
			continue
		}

		// Resolve channel constant name for the message log. We map provider
		// names to the existing models.Channel* constants where possible so
		// downstream filters (e.g. analytics) keep working.
		channelTag := p.Name()

		// Refresh-on-the-fly for providers that mint short-lived credentials
		// (LINE issues 30-day tokens we mint from channel_id + secret). The
		// EnsureCredentials method mutates conn.Credentials in place, so we
		// persist back to Mongo when refreshed=true.
		if r, ok := p.(channels.CredentialRefresher); ok {
			if refreshed, err := r.EnsureCredentials(ctx, conn); err != nil {
				log.Printf("webhook %s: refresh credentials: %v", p.Name(), err)
				_ = store.MarkError(ctx, conn.ID, err.Error())
				cancel()
				continue
			} else if refreshed {
				if err := store.UpdateCredentials(ctx, conn.ID, conn.Credentials); err != nil {
					log.Printf("webhook %s: persist refreshed credentials: %v", p.Name(), err)
				}
			}
		}

		for _, evt := range batch {
			conversationID := fmt.Sprintf("%s:%s:%s", p.Name(), externalID, evt.ExternalUserID)

			// Agent echo: the business replied to the customer outside topdee
			// (e.g. an admin typing in the Facebook Page inbox). Record it to
			// the transcript as a human turn and stop — no AI turn, no push.
			if evt.IsAgentEcho {
				if err := o.RecordAgentMessage(
					ctx, conn.TenantID, conversationID,
					channelTag, evt.ExternalUserID, evt.Text, evt.Attachments,
				); err != nil {
					log.Printf("webhook %s: record agent echo: %v", p.Name(), err)
				}
				go broadcastInboxUpdate(hub, m, conn.TenantID)
				continue
			}

			reply, _, _, err := o.HandleIncoming(
				ctx, conn.TenantID, conversationID,
				channelTag, evt.ExternalUserID, evt.Text, evt.Attachments,
			)
			if err != nil {
				log.Printf("webhook %s: orchestrator: %v", p.Name(), err)
				continue
			}

			// Backfill the customer's display name. Fire-and-forget — the
			// inbox can render with the placeholder if the profile API is
			// slow or returns 404 (user hasn't friended the bot, etc.).
			go cacheCustomerProfile(p, store, conn, evt)

			// Notify connected dashboard tabs that a new customer message
			// arrived so the inbox badge updates in real-time.
			go broadcastInboxUpdate(hub, m, conn.TenantID)

			if strings.TrimSpace(reply) == "" {
				continue
			}
			if err := p.Send(ctx, conn, evt, reply); err != nil {
				log.Printf("webhook %s: send: %v", p.Name(), err)
				_ = store.MarkError(ctx, conn.ID, err.Error())
			}
		}
		cancel()
	}
}

// broadcastInboxUpdate recomputes the unread count for a tenant and pushes
// an inbox_update event to all connected dashboard tabs via the Hub.
// Counts conversations where the customer spoke last OR needs_human is set.
func broadcastInboxUpdate(hub *realtime.Hub, m *db.Mongo, tenantID string) {
	if hub == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pipeline := []bson.M{
		{"$match": bson.M{
			"tenant_id": tenantID,
			"channel":   bson.M{"$ne": models.ChannelDashboard},
		}},
		{"$sort": bson.M{"created_at": -1}},
		{"$group": bson.M{
			"_id":              "$conversation_id",
			"last_sender_role": bson.M{"$first": "$role"},
		}},
		{"$lookup": bson.M{
			"from":         "conversations",
			"localField":   "_id",
			"foreignField": "_id",
			"as":           "conv_meta",
		}},
		{"$addFields": bson.M{
			"needs_human": bson.M{
				"$cond": []any{
					bson.M{"$gt": []any{bson.M{"$size": "$conv_meta"}, 0}},
					bson.M{"$arrayElemAt": []any{"$conv_meta.needs_human", 0}},
					false,
				},
			},
		}},
		{"$match": bson.M{"$or": []bson.M{
			{"last_sender_role": models.RoleUser},
			{"needs_human": true},
		}}},
		{"$count": "count"},
	}

	cur, err := m.DB.Collection("messages").Aggregate(ctx, pipeline)
	if err != nil {
		return
	}
	defer cur.Close(ctx)

	var result []struct {
		Count int `bson:"count"`
	}
	_ = cur.All(ctx, &result)

	count := 0
	if len(result) > 0 {
		count = result[0].Count
	}
	hub.Broadcast(tenantID, map[string]any{
		"type":  "inbox_update",
		"count": count,
	})
}

// ── Facebook OAuth callback ────────────────────────────────────────────
//
// This isn't a "webhook event" but it shares the public /webhooks tree
// because the redirect target lives at a stable, no-auth URL that's
// registered with Meta. We finish the OAuth dance here, persist the
// long-lived user token + page list keyed by `state`, and bounce the
// browser back to the dashboard for page selection.

func FacebookOAuthCallback(store *channels.Store, m *db.Mongo, cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		state := c.Query("state")
		code := c.Query("code")
		errMsg := c.Query("error_description")

		if state == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing state")
		}

		// Look up the in-flight handshake — proves we initiated it (and
		// gives us tenant + user to credit).
		st, err := store.GetOAuthState(c.Context(), state)
		if err != nil {
			return err
		}
		if st == nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid or expired state")
		}

		// User declined or Meta returned an error — bounce back to the
		// dashboard with a flag so the UI can show a message.
		if code == "" {
			_ = store.DeleteOAuthState(c.Context(), state)
			redirectTo := cfg.FrontendBaseURL + "/channels?fb_oauth=error&reason=" +
				url.QueryEscape(firstNonEmpty(errMsg, "no code returned"))
			return c.Redirect(redirectTo, fiber.StatusFound)
		}

		// Exchange the code for a long-lived user access token.
		userToken, err := channels.FacebookExchangeCode(c.Context(), cfg, code)
		if err != nil {
			log.Printf("fb oauth: exchange: %v", err)
			_ = store.DeleteOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?fb_oauth=error&reason=exchange_failed", fiber.StatusFound)
		}

		// List manageable pages. We persist the per-page access tokens on
		// the state record so the page-picker step doesn't have to re-fetch.
		pages, err := channels.FacebookListPages(c.Context(), userToken)
		if err != nil {
			log.Printf("fb oauth: list pages: %v", err)
			_ = store.DeleteOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?fb_oauth=error&reason=list_pages_failed", fiber.StatusFound)
		}

		st.UserAccessToken = userToken
		st.Pages = pages
		if err := store.SaveOAuthState(c.Context(), st); err != nil {
			log.Printf("fb oauth: save state: %v", err)
		}

		// Bounce to the dashboard so the user can pick which pages to wire up.
		return c.Redirect(cfg.FrontendBaseURL+"/channels?fb_oauth=ok&state="+url.QueryEscape(state), fiber.StatusFound)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ── Instagram OAuth callback ────────────────────────────────────────────
//
// Mirrors FacebookOAuthCallback. After Meta redirects back here, we exchange
// the code for a long-lived user token, discover all linked Instagram Business
// Accounts, store them on the state record, and bounce the browser to the
// dashboard account picker.

func InstagramOAuthCallback(store *channels.Store, m *db.Mongo, cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		state := c.Query("state")
		code := c.Query("code")
		errMsg := c.Query("error_description")

		if state == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing state")
		}

		st, err := store.GetIGOAuthState(c.Context(), state)
		if err != nil {
			return err
		}
		if st == nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid or expired state")
		}

		if code == "" {
			_ = store.DeleteIGOAuthState(c.Context(), state)
			redirectTo := cfg.FrontendBaseURL + "/channels?ig_oauth=error&reason=" +
				url.QueryEscape(firstNonEmpty(errMsg, "no code returned"))
			return c.Redirect(redirectTo, fiber.StatusFound)
		}

		userToken, err := channels.InstagramExchangeCode(c.Context(), cfg, code)
		if err != nil {
			log.Printf("ig oauth: exchange: %v", err)
			_ = store.DeleteIGOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?ig_oauth=error&reason=exchange_failed", fiber.StatusFound)
		}

		accounts, err := channels.InstagramListAccounts(c.Context(), userToken)
		if err != nil {
			log.Printf("ig oauth: list accounts: %v", err)
			_ = store.DeleteIGOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?ig_oauth=error&reason=list_accounts_failed", fiber.StatusFound)
		}

		st.UserAccessToken = userToken
		st.Accounts = accounts
		if err := store.SaveIGOAuthState(c.Context(), st); err != nil {
			log.Printf("ig oauth: save state: %v", err)
		}

		return c.Redirect(cfg.FrontendBaseURL+"/channels?ig_oauth=ok&state="+url.QueryEscape(state), fiber.StatusFound)
	}
}

// ── TikTok OAuth callback ──────────────────────────────────────────────
//
// Mirrors FacebookOAuthCallback. TikTok redirects here with ?code & ?state
// after the user authorizes the app. We exchange the code for an
// access/refresh token pair, discover the business accounts the user can
// manage, persist them on the state record, and bounce the browser back
// to the dashboard account picker.

func TikTokOAuthCallback(store *channels.Store, m *db.Mongo, cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		state := c.Query("state")
		code := c.Query("code")
		errMsg := firstNonEmpty(c.Query("error_description"), c.Query("error"))

		if state == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing state")
		}

		st, err := store.GetTTOAuthState(c.Context(), state)
		if err != nil {
			return err
		}
		if st == nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid or expired state")
		}

		if code == "" {
			_ = store.DeleteTTOAuthState(c.Context(), state)
			redirectTo := cfg.FrontendBaseURL + "/channels?tt_oauth=error&reason=" +
				url.QueryEscape(firstNonEmpty(errMsg, "no code returned"))
			return c.Redirect(redirectTo, fiber.StatusFound)
		}

		tok, err := channels.TikTokExchangeCode(c.Context(), cfg, code)
		if err != nil {
			log.Printf("tt oauth: exchange: %v", err)
			_ = store.DeleteTTOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?tt_oauth=error&reason=exchange_failed", fiber.StatusFound)
		}

		accounts, err := channels.TikTokListAccounts(c.Context(), tok.AccessToken, tok.OpenID)
		if err != nil {
			log.Printf("tt oauth: list accounts: %v", err)
			_ = store.DeleteTTOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?tt_oauth=error&reason=list_accounts_failed", fiber.StatusFound)
		}

		st.AccessToken = tok.AccessToken
		st.RefreshToken = tok.RefreshToken
		st.OpenID = tok.OpenID
		st.ExpiresIn = tok.ExpiresIn
		st.RefreshExpiresIn = tok.RefreshExpiresIn
		st.Accounts = accounts
		if err := store.SaveTTOAuthState(c.Context(), st); err != nil {
			log.Printf("tt oauth: save state: %v", err)
		}

		return c.Redirect(cfg.FrontendBaseURL+"/channels?tt_oauth=ok&state="+url.QueryEscape(state), fiber.StatusFound)
	}
}

// ── WhatsApp OAuth callback ────────────────────────────────────────────
//
// Mirrors FacebookOAuthCallback. After Meta redirects back here, we
// exchange the code for a long-lived user token, discover the WhatsApp
// Business Accounts + phone numbers, persist them on the state record,
// and bounce the browser back to the dashboard picker.

func WhatsAppOAuthCallback(store *channels.Store, m *db.Mongo, cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		state := c.Query("state")
		code := c.Query("code")
		errMsg := c.Query("error_description")

		if state == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing state")
		}

		st, err := store.GetWAOAuthState(c.Context(), state)
		if err != nil {
			return err
		}
		if st == nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid or expired state")
		}

		if code == "" {
			_ = store.DeleteWAOAuthState(c.Context(), state)
			redirectTo := cfg.FrontendBaseURL + "/channels?wa_oauth=error&reason=" +
				url.QueryEscape(firstNonEmpty(errMsg, "no code returned"))
			return c.Redirect(redirectTo, fiber.StatusFound)
		}

		userToken, err := channels.WhatsAppExchangeCode(c.Context(), cfg, code)
		if err != nil {
			log.Printf("wa oauth: exchange: %v", err)
			_ = store.DeleteWAOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?wa_oauth=error&reason=exchange_failed", fiber.StatusFound)
		}

		phoneNumbers, err := channels.WhatsAppListPhoneNumbers(c.Context(), userToken)
		if err != nil {
			log.Printf("wa oauth: list phone numbers: %v", err)
			_ = store.DeleteWAOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?wa_oauth=error&reason=list_phone_numbers_failed", fiber.StatusFound)
		}

		st.UserAccessToken = userToken
		st.PhoneNumbers = phoneNumbers
		if err := store.SaveWAOAuthState(c.Context(), st); err != nil {
			log.Printf("wa oauth: save state: %v", err)
		}

		return c.Redirect(cfg.FrontendBaseURL+"/channels?wa_oauth=ok&state="+url.QueryEscape(state), fiber.StatusFound)
	}
}

// ── Lazada OAuth callback ──────────────────────────────────────────────
//
// Lazada doesn't have a picker step — one OAuth handshake binds to one
// seller, so we finish the connection right here. Country comes back as
// a query param and we persist it on the connection so the regional API
// host can be resolved on every subsequent call.

func LazadaOAuthCallback(store *channels.Store, m *db.Mongo, cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		state := c.Query("state")
		code := c.Query("code")
		country := strings.ToLower(strings.TrimSpace(c.Query("country")))
		errMsg := c.Query("error_description")

		if state == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing state")
		}

		st, err := store.GetLZOAuthState(c.Context(), state)
		if err != nil {
			return err
		}
		if st == nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid or expired state")
		}

		if code == "" {
			_ = store.DeleteLZOAuthState(c.Context(), state)
			redirectTo := cfg.FrontendBaseURL + "/channels?lz_oauth=error&reason=" +
				url.QueryEscape(firstNonEmpty(errMsg, "no code returned"))
			return c.Redirect(redirectTo, fiber.StatusFound)
		}

		tok, err := channels.LazadaExchangeCode(c.Context(), cfg, code)
		if err != nil {
			log.Printf("lz oauth: exchange: %v", err)
			_ = store.DeleteLZOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?lz_oauth=error&reason=exchange_failed", fiber.StatusFound)
		}

		// Pick the first country binding when the callback didn't tell
		// us which one to use. Most apps stick to a single country.
		sellerID := tok.Account
		shortCode := ""
		if country == "" && len(tok.CountryUserInfo) > 0 {
			country = strings.ToLower(tok.CountryUserInfo[0].Country)
		}
		for _, cu := range tok.CountryUserInfo {
			if strings.EqualFold(cu.Country, country) {
				if cu.SellerID != "" {
					sellerID = cu.SellerID
				}
				shortCode = cu.ShortCode
				break
			}
		}
		if sellerID == "" {
			sellerID = tok.Account
		}

		store2 := store // alias for readability
		now := time.Now()
		accessExp := now.Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
		refreshExp := now.Add(time.Duration(tok.RefreshExpiresIn) * time.Second).UTC().Format(time.RFC3339)

		displayName := tok.Account
		if shortCode != "" {
			displayName = shortCode
		}
		if displayName == "" {
			displayName = "Lazada Seller " + sellerID
		}

		conn := &models.ChannelConnection{
			TenantID:    st.TenantID,
			Provider:    models.ProviderLazada,
			ExternalID:  sellerID,
			DisplayName: displayName,
			Credentials: map[string]string{
				"access_token":             tok.AccessToken,
				"refresh_token":            tok.RefreshToken,
				"access_token_expires_at":  accessExp,
				"refresh_token_expires_at": refreshExp,
				"country":                  country,
				"app_key":                  cfg.LZAppKey,
				"app_secret":               cfg.LZAppSecret,
			},
			Config: map[string]any{
				"country":    country,
				"short_code": shortCode,
			},
			Status:    models.ChannelStatusActive,
			CreatedBy: st.UserID,
		}
		if err := store2.Upsert(c.Context(), conn); err != nil {
			log.Printf("lz oauth: upsert: %v", err)
			_ = store.DeleteLZOAuthState(c.Context(), state)
			return c.Redirect(cfg.FrontendBaseURL+"/channels?lz_oauth=error&reason=upsert_failed", fiber.StatusFound)
		}

		_ = store.DeleteLZOAuthState(c.Context(), state)
		return c.Redirect(cfg.FrontendBaseURL+"/channels?lz_oauth=ok&seller_id="+url.QueryEscape(sellerID), fiber.StatusFound)
	}
}

// cacheCustomerProfile fetches a customer's display name from the platform
// (LINE only for now — Meta tightly restricts profile access on Messenger
// and most page-scoped IDs return nothing useful) and caches it. Idempotent;
// safe to call on every inbound message — the caller decides how often.
//
// Best-effort: any error is logged and swallowed. The inbox falls back to
// "LINE User abcd12" when no profile is cached.
func cacheCustomerProfile(p channels.Provider, store *channels.Store, conn *models.ChannelConnection, evt channels.ParsedEvent) {
	if evt.ExternalUserID == "" {
		return
	}

	provider := p.Name()
	if provider != models.ProviderLine && provider != models.ProviderFacebook {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Skip the network round-trip if we already have a fresh profile.
	if existing, _ := store.GetProfile(ctx, provider, evt.ExternalUserID); existing != nil {
		if time.Since(existing.UpdatedAt) < 7*24*time.Hour {
			return
		}
	}

	var displayName, pictureURL, language string

	switch provider {
	case models.ProviderLine:
		token := conn.Credentials["channel_access_token"]
		if token == "" {
			return
		}
		resp, err := channels.LineUserProfile(ctx, token, evt.ExternalUserID)
		if err != nil {
			log.Printf("inbox: LINE profile fetch: %v", err)
			return
		}
		if resp == nil || resp.DisplayName == "" {
			return
		}
		displayName = resp.DisplayName
		pictureURL = resp.PictureURL
		language = resp.Language

	case models.ProviderFacebook:
		token := conn.Credentials["page_access_token"]
		if token == "" {
			return
		}
		resp, err := channels.FacebookUserProfile(ctx, token, evt.ExternalUserID)
		if err != nil {
			log.Printf("inbox: Facebook profile fetch: %v", err)
			return
		}
		if resp == nil || resp.DisplayName == "" {
			// Meta restricts profile access in many cases — fail silently.
			return
		}
		displayName = resp.DisplayName
		pictureURL = resp.PictureURL
	}

	if displayName == "" {
		return
	}
	_ = store.UpsertProfile(ctx, &models.CustomerProfile{
		Provider:       provider,
		ExternalUserID: evt.ExternalUserID,
		DisplayName:    displayName,
		PictureURL:     pictureURL,
		Language:       language,
	})
}
