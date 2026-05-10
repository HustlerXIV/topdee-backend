package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port         string
	MongoURI     string
	MongoDB      string
	JWTSecret    string
	JWTTTLHours  int
	AIServiceURL string

	// Platform agent — applied uniformly to every tenant. Customers don't
	// see or change this; they only upload knowledge and connect channels.
	PlatformSystemPrompt string
	PlatformModel        string
	PlatformTemperature  float64

	// Facebook Messenger app-level secrets + OAuth credentials. The page id
	// and page access token are per-tenant and stored in channel_connections.
	//
	//   • FBAppID + FBAppSecret — the Meta app's identity. Used to sign
	//     webhook payloads (signature verification) and to perform the
	//     Facebook Login OAuth dance when a tenant connects new pages.
	//   • FBVerifyToken — the shared secret Meta uses to verify ownership of
	//     the webhook URL during the GET handshake.
	//   • FBOAuthRedirectURI — must match the URL configured in the Meta
	//     developer console under "Facebook Login → Valid OAuth Redirect URIs".
	//     Typically `${BACKEND_PUBLIC_URL}/webhooks/facebook/oauth/callback`.
	FBAppID            string
	FBAppSecret        string
	FBVerifyToken      string
	FBOAuthRedirectURI string

	// FrontendBaseURL — used to build the post-OAuth redirect that brings
	// the user back to the dashboard's channels page. Typically the same
	// origin as AcceptInviteBaseURL minus the path.
	FrontendBaseURL string

	// BackendPublicURL — public origin of THIS service, used to build the
	// per-channel webhook URLs we hand back to customers (LINE, etc.).
	// Almost always different from FrontendBaseURL: the frontend is on the
	// dashboard origin, the backend needs a publicly reachable URL the
	// platform can POST to. In dev, ngrok / cloudflared.
	BackendPublicURL string

	// Public URL used to build invite-acceptance links the inviter shares
	// with the recipient. Set to your frontend's /accept-invite route.
	AcceptInviteBaseURL string

	// Emails that get auto-promoted to platform admin on first register.
	// Comma-separated env var. Useful for bootstrapping the very first
	// Topdee staff account; once one admin exists they can promote others
	// via the admin UI.
	BootstrapAdminEmails []string

	// Resend — transactional email (https://resend.com).
	// Sign up, verify your domain, then paste the API key here.
	// If empty, invite emails are skipped and the accept_url is returned in the
	// API response so you can share it manually during development.
	ResendAPIKey string
	// EmailFrom is the "From" address for outbound emails.
	// Must match a domain verified in your Resend account.
	// Example: "Topdee <noreply@mail.yourdomain.com>"
	EmailFrom string

	// Stripe — payment processing. Set via env. The webhook secret comes
	// from the Stripe dashboard's "Reveal" on the endpoint page; keep it
	// in env, never hard-coded.
	//
	// Per-plan Stripe price IDs (price_xxx) are stored on each Plan document
	// in MongoDB and managed via Admin → Plans, so they no longer need to be
	// env vars.
	StripeSecretKey     string
	StripeWebhookSecret string
	BillingReturnURL    string // where Checkout + Portal redirect back to
}

func Load() (*Config, error) {
	c := &Config{
		Port:         getEnv("BACKEND_PORT", "8080"),
		MongoURI:     getEnv("MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:      getEnv("MONGO_DB", "topdee"),
		JWTSecret:    getEnv("JWT_SECRET", ""),
		AIServiceURL: getEnv("AI_SERVICE_URL", "http://localhost:8000"),

		PlatformSystemPrompt: getEnv("PLATFORM_SYSTEM_PROMPT",
			"You are a helpful, friendly customer support agent. Answer concisely "+
				"using only the provided knowledge. If the answer is not in the knowledge, "+
				"say you don't know and offer to connect the customer with a human."),
		PlatformModel: getEnv("PLATFORM_MODEL", "gemini-2.5-flash"),

		FBAppID:            getEnv("FB_APP_ID", ""),
		FBAppSecret:        getEnv("FB_APP_SECRET", ""),
		FBVerifyToken:      getEnv("FB_VERIFY_TOKEN", ""),
		FBOAuthRedirectURI: getEnv("FB_OAUTH_REDIRECT_URI", "http://localhost:8080/webhooks/facebook/oauth/callback"),

		FrontendBaseURL:  getEnv("FRONTEND_BASE_URL", "http://localhost:3000"),
		BackendPublicURL: getEnv("BACKEND_PUBLIC_URL", "http://localhost:8080"),

		AcceptInviteBaseURL: getEnv("ACCEPT_INVITE_BASE_URL", "http://localhost:3000/accept-invite"),

		BootstrapAdminEmails: parseEmailList(getEnv("BOOTSTRAP_ADMIN_EMAILS", "")),

		ResendAPIKey: getEnv("RESEND_API_KEY", ""),
		EmailFrom:    getEnv("EMAIL_FROM", "Topdee <noreply@example.com>"),

		StripeSecretKey:     getEnv("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret: getEnv("STRIPE_WEBHOOK_SECRET", ""),
		BillingReturnURL:    getEnv("BILLING_RETURN_URL", "http://localhost:3000/billing"),
	}
	ttl, _ := strconv.Atoi(getEnv("JWT_TTL_HOURS", "24"))
	if ttl <= 0 {
		ttl = 24
	}
	c.JWTTTLHours = ttl

	temp, err := strconv.ParseFloat(getEnv("PLATFORM_TEMPERATURE", "0.3"), 64)
	if err != nil {
		temp = 0.3
	}
	c.PlatformTemperature = temp

	if c.JWTSecret == "" {
		return nil, errors.New("JWT_SECRET is required")
	}
	return c, nil
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseEmailList splits a comma-separated env var into normalized emails.
// Lower-cased + trimmed. Empty strings ignored.
func parseEmailList(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		e := strings.ToLower(strings.TrimSpace(p))
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}
