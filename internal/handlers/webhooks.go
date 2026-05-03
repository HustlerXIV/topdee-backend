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

	"github.com/topdee/backend/internal/channels"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/models"
)

// WebhookHandler returns the generic GET/POST handler for /webhooks/:provider.
//
// Both verbs share the same Fiber handler — we dispatch on c.Method() so the
// router only needs one entry per provider.
func WebhookHandler(reg *channels.Registry, store *channels.Store, m *db.Mongo, o *Orchestrator, cfg *config.Config) fiber.Handler {
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
		go processEvents(p, store, m, o, cfg, headers, body, events)
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
	// Only LINE for now. Add per-provider profile fetchers here as we
	// build them out.
	if p.Name() != models.ProviderLine {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Skip the network round-trip if we already have a profile that's
	// less than 7 days old.
	if existing, _ := store.GetProfile(ctx, p.Name(), evt.ExternalUserID); existing != nil {
		if time.Since(existing.UpdatedAt) < 7*24*time.Hour {
			return
		}
	}

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
		// User isn't a friend / blocked profile sharing — nothing to cache.
		return
	}
	_ = store.UpsertProfile(ctx, &models.CustomerProfile{
		Provider:       p.Name(),
		ExternalUserID: evt.ExternalUserID,
		DisplayName:    resp.DisplayName,
		PictureURL:     resp.PictureURL,
		Language:       resp.Language,
	})
}
