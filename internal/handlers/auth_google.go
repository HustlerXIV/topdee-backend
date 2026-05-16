package handlers

// Google OAuth 2.0 / OpenID Connect login.
//
// Flow:
//   1. GET /api/v1/auth/google/start
//      → sets a signed `g_state` cookie, redirects browser to Google consent.
//   2. Google redirects to GET /api/v1/auth/google/callback?code=…&state=…
//      → exchange code for tokens, fetch profile, find-or-create user/tenant,
//        issue JWT, redirect browser to {FRONTEND_BASE_URL}/auth/google/callback?token=JWT[&new=true]
//
// No external OAuth library is needed — just standard net/http + encoding/json.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/topdee/backend/internal/auth"
	"github.com/topdee/backend/internal/config"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/models"
)

type GoogleAuthHandler struct {
	mongo *db.Mongo
	cfg   *config.Config
}

func NewGoogleAuthHandler(m *db.Mongo, cfg *config.Config) *GoogleAuthHandler {
	return &GoogleAuthHandler{mongo: m, cfg: cfg}
}

// ── Start ─────────────────────────────────────────────────────────────────────

// Start redirects the browser to Google's consent screen.
// GET /api/v1/auth/google/start
func (h *GoogleAuthHandler) Start(c *fiber.Ctx) error {
	state, err := newState()
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not generate state")
	}

	// Sign the state with the JWT secret so we can verify it on callback
	// without keeping server-side session state.
	signed := signState(state, h.cfg.JWTSecret)

	// Store the signed state in an HTTP-only cookie (30-min TTL).
	c.Cookie(&fiber.Cookie{
		Name:     "g_state",
		Value:    signed,
		HTTPOnly: true,
		SameSite: "Lax",
		MaxAge:   1800,
		Secure:   strings.HasPrefix(h.cfg.FrontendBaseURL, "https"),
	})

	authURL := "https://accounts.google.com/o/oauth2/v2/auth?" + url.Values{
		"client_id":     {h.cfg.GoogleClientID},
		"redirect_uri":  {h.cfg.GoogleOAuthRedirectURI},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
		"access_type":   {"offline"},
		"prompt":        {"select_account"},
	}.Encode()

	return c.Redirect(authURL, fiber.StatusTemporaryRedirect)
}

// ── Callback ──────────────────────────────────────────────────────────────────

// Callback handles the code Google sends back.
// GET /api/v1/auth/google/callback?code=…&state=…
func (h *GoogleAuthHandler) Callback(c *fiber.Ctx) error {
	frontendCallback := h.cfg.FrontendBaseURL + "/auth/google/callback"

	// ── Verify state ──────────────────────────────────────────────────
	cookieState := c.Cookies("g_state")
	queryState := c.Query("state")
	if cookieState == "" || queryState == "" || !verifyState(queryState, cookieState, h.cfg.JWTSecret) {
		return c.Redirect(frontendCallback+"?error=state_mismatch", fiber.StatusTemporaryRedirect)
	}
	// Clear the cookie.
	c.Cookie(&fiber.Cookie{Name: "g_state", Value: "", MaxAge: -1, HTTPOnly: true})

	if errMsg := c.Query("error"); errMsg != "" {
		return c.Redirect(frontendCallback+"?error="+url.QueryEscape(errMsg), fiber.StatusTemporaryRedirect)
	}

	code := c.Query("code")
	if code == "" {
		return c.Redirect(frontendCallback+"?error=missing_code", fiber.StatusTemporaryRedirect)
	}

	// ── Exchange code for tokens ──────────────────────────────────────
	profile, err := h.fetchGoogleProfile(code)
	if err != nil {
		return c.Redirect(frontendCallback+"?error="+url.QueryEscape(err.Error()), fiber.StatusTemporaryRedirect)
	}
	if profile.Email == "" {
		return c.Redirect(frontendCallback+"?error=no_email", fiber.StatusTemporaryRedirect)
	}

	// ── Find or create user ───────────────────────────────────────────
	token, isNew, err := h.findOrCreateUser(c.Context(), profile)
	if err != nil {
		return c.Redirect(frontendCallback+"?error="+url.QueryEscape(err.Error()), fiber.StatusTemporaryRedirect)
	}

	redirectURL := frontendCallback + "?token=" + url.QueryEscape(token)
	if isNew {
		redirectURL += "&new=true"
	}
	return c.Redirect(redirectURL, fiber.StatusTemporaryRedirect)
}

// ── Google profile ─────────────────────────────────────────────────────────

type googleProfile struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	Picture       string `json:"picture"`
}

func (h *GoogleAuthHandler) fetchGoogleProfile(code string) (*googleProfile, error) {
	// Exchange code → access token.
	resp, err := http.PostForm("https://oauth2.googleapis.com/token", url.Values{
		"code":          {code},
		"client_id":     {h.cfg.GoogleClientID},
		"client_secret": {h.cfg.GoogleClientSecret},
		"redirect_uri":  {h.cfg.GoogleOAuthRedirectURI},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("token parse: %w", err)
	}
	if tokenResp.Error != "" {
		return nil, fmt.Errorf("google error: %s — %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	// Fetch profile with the access token.
	req, _ := http.NewRequest("GET", "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	profileResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	defer profileResp.Body.Close()
	profileBody, _ := io.ReadAll(profileResp.Body)

	var p googleProfile
	if err := json.Unmarshal(profileBody, &p); err != nil {
		return nil, fmt.Errorf("userinfo parse: %w", err)
	}
	return &p, nil
}

// ── Find or create ────────────────────────────────────────────────────────────

func (h *GoogleAuthHandler) findOrCreateUser(ctx context.Context, p *googleProfile) (token string, isNew bool, err error) {
	// Look up by email.
	var u models.User
	dbErr := h.mongo.DB.Collection("users").
		FindOne(ctx, bson.M{"email": strings.ToLower(p.Email)}).Decode(&u)

	if dbErr == nil {
		// Existing user — just issue a token.
		if u.Suspended {
			return "", false, fmt.Errorf("account suspended")
		}
		tok, err := auth.IssueToken(auth.IssueOpts{
			Secret: h.cfg.JWTSecret, UserID: u.ID, TenantID: u.TenantID,
			Email: u.Email, Role: u.Role, IsAdmin: u.IsPlatformAdmin,
			TTLHours: h.cfg.JWTTTLHours,
		})
		return tok, false, err
	}
	if dbErr != mongo.ErrNoDocuments {
		return "", false, dbErr
	}

	// New user — create tenant + owner.
	now := time.Now().UTC()
	tenantName := p.Name
	if tenantName == "" {
		tenantName = strings.Split(p.Email, "@")[0]
	}

	// Auto-apply trial subscription if a free plan exists.
	var initialSubscription *models.Subscription
	var freePlan struct {
		ExpiryDays int `bson:"expiry_days"`
	}
	if err := h.mongo.DB.Collection("plans").
		FindOne(ctx, bson.M{"_id": "free"}).Decode(&freePlan); err == nil {
		if freePlan.ExpiryDays > 0 {
			trialEnd := now.AddDate(0, 0, freePlan.ExpiryDays)
			initialSubscription = &models.Subscription{
				Status:      models.SubStatusTrialing,
				TrialEndsAt: &trialEnd,
				UpdatedAt:   now,
			}
		}
	}

	tenant := models.Tenant{
		ID:           uuid.NewString(),
		Name:         tenantName,
		Plan:         "free",
		Subscription: initialSubscription,
		CreatedAt:    now,
	}
	if _, err := h.mongo.DB.Collection("tenants").InsertOne(ctx, tenant); err != nil {
		return "", false, err
	}

	newUser := models.User{
		ID:              uuid.NewString(),
		TenantID:        tenant.ID,
		Name:            p.Name,
		Email:           strings.ToLower(p.Email),
		PasswordHash:    "", // no password — Google is the auth provider
		Role:            "owner",
		IsPlatformAdmin: false,
		CreatedAt:       now,
	}
	if _, err := h.mongo.DB.Collection("users").InsertOne(ctx, newUser); err != nil {
		return "", false, err
	}

	tok, err := auth.IssueToken(auth.IssueOpts{
		Secret: h.cfg.JWTSecret, UserID: newUser.ID, TenantID: tenant.ID,
		Email: newUser.Email, Role: newUser.Role, IsAdmin: false,
		TTLHours: h.cfg.JWTTTLHours,
	})
	return tok, true, err
}

// ── State helpers ─────────────────────────────────────────────────────────────

func newState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// signState returns HMAC-SHA256(state, secret) + "." + state so we can
// verify it on callback without storing anything server-side.
func signState(state, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(state))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return sig + "." + state
}

// verifyState checks that the query-param state matches the signed cookie.
func verifyState(queryState, cookieSigned, secret string) bool {
	parts := strings.SplitN(cookieSigned, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expected := signState(parts[1], secret)
	return hmac.Equal([]byte(cookieSigned), []byte(expected)) && parts[1] == queryState
}
