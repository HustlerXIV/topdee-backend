package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed IANA timezone database so Alpine containers work

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/joho/godotenv"

	"github.com/topdee/backend/internal/channels"
	"github.com/topdee/backend/internal/clients"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/handlers"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/realtime"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mongo, err := db.Connect(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Fatalf("mongo connect: %v", err)
	}
	defer func() {
		_ = mongo.Client.Disconnect(context.Background())
	}()

	aiClient := clients.NewAIClient(cfg.AIServiceURL)

	hub := realtime.NewHub()

	orch := handlers.NewOrchestrator(mongo, aiClient, cfg, hub)

	// Channel provider registry — adding a new social platform is one
	// `registry.Register(NewFooProvider())` line.
	channelRegistry := channels.NewRegistry()
	channelRegistry.Register(channels.NewFacebookProvider())
	channelRegistry.Register(channels.NewInstagramProvider())
	channelRegistry.Register(channels.NewLineProvider())
	channelStore := channels.NewStore(mongo)

	// One-time migration: copy any legacy tenants.facebook / tenants.line
	// sub-documents into the new channel_connections collection. Idempotent.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		fbN, lineN, errN := channels.MigrateLegacyTenantConnections(ctx, mongo)
		cancel()
		if fbN+lineN+errN > 0 {
			log.Printf("channels: migrated %d facebook + %d line legacy connections (errors=%d)", fbN, lineN, errN)
		}
	}

	// Seed default plans if the collection is empty.
	if err := handlers.SeedDefaultPlans(mongo); err != nil {
		log.Printf("plans: seed error (non-fatal): %v", err)
	}

	app := fiber.New(fiber.Config{
		AppName:      "topdee-backend",
		BodyLimit:    25 * 1024 * 1024, // 25MB to allow PDF uploads
		ErrorHandler: handlers.ErrorHandler,
	})
	app.Use(recover.New())
	app.Use(logger.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins: cfg.AllowOrigins,
		AllowHeaders: "Origin, Content-Type, Accept, Authorization",
		AllowMethods: "GET, POST, PUT, PATCH, DELETE, OPTIONS",
	}))

	// public
	app.Get("/health", handlers.Health(mongo, aiClient))

	// WebSocket — /ws?token=<jwt>
	// Browsers can't send Authorization headers on WS connections so we
	// authenticate via the JWT query param. The hub fans out inbox_update
	// events to all connected dashboard tabs in the same tenant.
	app.Use("/ws", handlers.WSUpgrade())
	app.Get("/ws", handlers.WSHandler(hub, cfg))

	api := app.Group("/api/v1")

	// auth routes (public)
	authH := handlers.NewAuthHandler(mongo, cfg)
	api.Post("/auth/register", authH.Register)
	api.Post("/auth/login", authH.Login)
	api.Post("/auth/forgot-password", authH.ForgotPassword)
	api.Post("/auth/reset-password", authH.ResetPassword)

	// Google OAuth — public, browser-redirect flow.
	googleH := handlers.NewGoogleAuthHandler(mongo, cfg)
	api.Get("/auth/google/start", googleH.Start)
	api.Get("/auth/google/callback", googleH.Callback)

	// Public plans — no auth, used by homepage and billing page.
	api.Get("/plans", handlers.PublicPlans(mongo))

	// Public accept-invite — exchanges a token for a new user + JWT.
	teamH := handlers.NewTeamHandler(mongo, cfg)
	api.Get("/auth/invite-info", teamH.InviteInfo)
	api.Post("/auth/accept-invite", teamH.AcceptInvite)

	// Auth-gated routes. Once `api.Use(RequireAuth)` runs, every subsequent
	// route on this group inherits the JWT check.
	protected := api.Use(middleware.RequireAuth(cfg))

	// Platform admin routes (Topdee staff). Inherit RequireAuth from the
	// chain above; we only add the admin guard here.
	adminH := handlers.NewAdminHandler(mongo)
	protected.Get("/admin/metrics", middleware.RequireAdmin(), adminH.Metrics)
	protected.Get("/admin/tenants", middleware.RequireAdmin(), adminH.ListTenants)
	protected.Get("/admin/tenants/:id", middleware.RequireAdmin(), adminH.GetTenant)
	protected.Patch("/admin/tenants/:id", middleware.RequireAdmin(), adminH.UpdateTenant)
	protected.Patch("/admin/tenants/:id/subscription", middleware.RequireAdmin(), adminH.UpdateSubscription)
	protected.Post("/admin/tenants/:id/subscription/extend", middleware.RequireAdmin(), adminH.ExtendSubscription)
	protected.Delete("/admin/tenants/:id", middleware.RequireAdmin(), adminH.DeleteTenant)
	protected.Get("/admin/users", middleware.RequireAdmin(), adminH.ListUsers)
	protected.Patch("/admin/users/:id", middleware.RequireAdmin(), adminH.UpdateUser)
	protected.Delete("/admin/users/:id", middleware.RequireAdmin(), adminH.DeleteUser)
	protected.Get("/admin/plans", middleware.RequireAdmin(), adminH.ListPlans)
	protected.Post("/admin/plans", middleware.RequireAdmin(), adminH.CreatePlan)
	protected.Get("/admin/plans/:id", middleware.RequireAdmin(), adminH.GetPlan)
	protected.Put("/admin/plans/:id", middleware.RequireAdmin(), adminH.UpdatePlan)
	protected.Delete("/admin/plans/:id", middleware.RequireAdmin(), adminH.DeletePlan)

	// Knowledge bases
	kbH := handlers.NewKnowledgeHandler(mongo, aiClient)
	protected.Get("/knowledge", kbH.List)
	protected.Post("/knowledge", kbH.Create)
	protected.Get("/knowledge/:id", kbH.Get)
	protected.Delete("/knowledge/:id", kbH.Delete)
	protected.Post("/knowledge/:id/files", kbH.UploadFile)

	// Bot settings (per-tenant override of the platform agent)
	botH := handlers.NewBotHandler(mongo, cfg)
	protected.Get("/bot", botH.Get)
	protected.Put("/bot", botH.Update)

	// Settings — current user's account, password, and workspace profile.
	settingsH := handlers.NewSettingsHandler(mongo, cfg)
	protected.Get("/settings", settingsH.Get)
	protected.Patch("/settings/account", settingsH.UpdateAccount)
	protected.Patch("/settings/password", settingsH.UpdatePassword)
	protected.Patch("/settings/workspace", settingsH.UpdateWorkspace)
	protected.Post("/settings/workspace/logo", settingsH.UploadLogo)
	protected.Patch("/settings/notifications", settingsH.UpdateNotifications)

	// Team — members + invites. Anyone in the workspace can list members,
	// but write operations are gated to owner/admin via RequireRole.
	manage := middleware.RequireRole("owner", "admin")
	protected.Get("/team/members", teamH.ListMembers)
	protected.Patch("/team/members/:id", middleware.RequireRole("owner"), teamH.UpdateMemberRole)
	protected.Delete("/team/members/:id", manage, teamH.RemoveMember)
	protected.Get("/team/invites", manage, teamH.ListInvites)
	protected.Post("/team/invites", manage, teamH.CreateInvite)
	protected.Delete("/team/invites/:id", manage, teamH.RevokeInvite)
	protected.Post("/team/invites/:id/resend", manage, teamH.ResendInvite)

	// Business hours (drives "Are we open right now?" hint into the AI prompt)
	bhH := handlers.NewBusinessHoursHandler(mongo)
	protected.Get("/business-hours", bhH.Get)
	protected.Put("/business-hours", bhH.Update)

	// Channel connections — generic, multi-account per provider.
	chH := handlers.NewChannelsHandler(mongo, cfg)
	protected.Get("/channels", chH.List)
	protected.Get("/channels/webhook-url-template", chH.WebhookURLTemplate)
	protected.Delete("/channels/:id", chH.Disconnect)
	// LINE: manual paste-in (no OAuth on LINE).
	protected.Put("/channels/line", chH.ConnectLine)
	// Facebook: OAuth login flow → page picker → connect selected pages.
	protected.Post("/channels/facebook/oauth/start", chH.FacebookOAuthStart)
	protected.Get("/channels/facebook/oauth/pages", chH.FacebookOAuthPages)
	protected.Post("/channels/facebook/oauth/connect", chH.FacebookOAuthConnect)
	// Instagram: OAuth login flow → account picker → connect selected IG Business Accounts.
	protected.Post("/channels/instagram/oauth/start", chH.InstagramOAuthStart)
	protected.Get("/channels/instagram/oauth/accounts", chH.InstagramOAuthAccounts)
	protected.Post("/channels/instagram/oauth/connect", chH.InstagramOAuthConnect)

	// Analytics — real stats from the messages collection.
	analyticsH := handlers.NewAnalyticsHandler(mongo)
	protected.Get("/analytics", analyticsH.GetStats)

	// Playground (in-dashboard test chat)
	pgH := handlers.NewPlaygroundHandler(orch, mongo)
	protected.Post("/playground/chat", pgH.Send)
	protected.Get("/playground/conversations", pgH.ListConversations)
	protected.Get("/playground/conversations/:id", pgH.GetConversation)

	// Inbox — real customer conversations from LINE / Facebook / etc.
	// (Playground messages are excluded server-side.) The handler needs
	// the channel registry + store too so it can dispatch outbound
	// human-agent replies through the right provider's push API.
	inboxH := handlers.NewInboxHandler(mongo, channelRegistry, channelStore, hub, cfg)
	protected.Get("/inbox/unread-count", inboxH.UnreadCount)
	protected.Get("/inbox/conversations", inboxH.ListConversations)
	protected.Get("/inbox/media/:id", inboxH.GetMedia)
	protected.Get("/inbox/conversations/:id/messages", inboxH.GetMessages)
	protected.Post("/inbox/conversations/:id/messages", inboxH.SendMessage)
	protected.Post("/inbox/conversations/:id/images", inboxH.SendImage)
	protected.Patch("/inbox/conversations/:id/resolve", inboxH.ResolveHandoff)

	// Referral programme — user-facing.
	referralH := handlers.NewReferralHandler(mongo)
	protected.Get("/referral/code", referralH.GetCode)
	protected.Get("/referral", referralH.GetStats)
	protected.Get("/referral/wallet", referralH.GetWallet)
	protected.Post("/referral/wallet/payout-request", referralH.SubmitPayoutRequest)
	protected.Get("/referral/wallet/payout-requests", referralH.GetMyPayoutRequests)

	// Referral programme — platform admin.
	adminReferralH := handlers.NewAdminReferralHandler(mongo)
	protected.Get("/admin/referral/settings", middleware.RequireAdmin(), adminReferralH.GetSettings)
	protected.Put("/admin/referral/settings", middleware.RequireAdmin(), adminReferralH.UpdateSettings)
	protected.Get("/admin/referral/referrals", middleware.RequireAdmin(), adminReferralH.ListReferrals)
	protected.Get("/admin/referral/wallets", middleware.RequireAdmin(), adminReferralH.ListWallets)
	protected.Post("/admin/referral/wallets/:id/payout", middleware.RequireAdmin(), adminReferralH.MarkPayoutDone)
	protected.Patch("/admin/referral/wallets/:id", middleware.RequireAdmin(), adminReferralH.UpdateWalletPayoutType)
	protected.Get("/admin/referral/payout-requests", middleware.RequireAdmin(), adminReferralH.ListPayoutRequests)
	protected.Post("/admin/referral/payout-requests/:id/approve", middleware.RequireAdmin(), adminReferralH.ApprovePayoutRequest)
	protected.Post("/admin/referral/payout-requests/:id/reject", middleware.RequireAdmin(), adminReferralH.RejectPayoutRequest)

	// Stripe billing — tenant-scoped self-service.
	billingH := handlers.NewBillingHandler(mongo, cfg)
	protected.Get("/billing", billingH.GetInfo)
	protected.Post("/billing/checkout-session", billingH.CreateCheckoutSession)
	protected.Post("/billing/portal-session", billingH.CreatePortalSession)
	protected.Get("/billing/payment-methods", billingH.ListPaymentMethods)
	protected.Delete("/billing/payment-methods/:id", billingH.RemovePaymentMethod)
	protected.Post("/billing/cancel", billingH.CancelSubscription)
	protected.Post("/billing/reactivate", billingH.ReactivateSubscription)
	protected.Post("/billing/sync-session", billingH.SyncCheckoutSession)
	protected.Post("/billing/promptpay-checkout", billingH.CreatePromptPayCheckout)
	protected.Get("/billing/invoices", billingH.ListInvoices)

	// Public webhooks (secured by signature verification). The generic
	// /webhooks/:provider route covers every social platform via the
	// registry; Stripe and the Facebook OAuth callback get explicit
	// routes since they don't fit the per-provider Provider interface.
	webhooks := app.Group("/webhooks")
	webhooks.Get("/facebook/oauth/callback", handlers.FacebookOAuthCallback(channelStore, mongo, cfg))
	webhooks.Get("/instagram/oauth/callback", handlers.InstagramOAuthCallback(channelStore, mongo, cfg))
	webhooks.Post("/stripe", handlers.StripeWebhook(mongo, cfg))
	// Generic provider webhooks. Two URL shapes are supported:
	//   /webhooks/<provider>                 — uses the body to find the
	//                                          right connection (Facebook).
	//   /webhooks/<provider>/<external_id>   — per-connection URL pasted
	//                                          into the customer's console
	//                                          (LINE channel_id, etc.).
	webhookHandler := handlers.WebhookHandler(channelRegistry, channelStore, mongo, orch, cfg, hub)
	webhooks.Get("/:provider", webhookHandler)
	webhooks.Post("/:provider", webhookHandler)
	webhooks.Get("/:provider/:external_id", webhookHandler)
	webhooks.Post("/:provider/:external_id", webhookHandler)

	// Background subscription expiry sweep.
	//
	// Runs once at startup (to catch any lapsed tenants from downtime) and
	// then every hour. This is the safety net for missed Stripe webhooks and
	// for admin-assigned plans with expiry_days.
	go func() {
		run := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			n, err := handlers.ExpireSubscriptions(ctx, mongo)
			if err != nil {
				log.Printf("[expiry] sweep error: %v", err)
			} else if n > 0 {
				log.Printf("[expiry] downgraded %d tenant(s) to free plan", n)
			}
		}
		run() // immediate pass on startup
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			run()
		}
	}()

	// graceful shutdown
	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := app.Listen(":" + cfg.Port); err != nil {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down")
	_ = app.ShutdownWithTimeout(5 * time.Second)
}
