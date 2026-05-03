package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/topdee/backend/internal/auth"
	"github.com/topdee/backend/internal/config"
)

const (
	CtxUserID   = "uid"
	CtxTenantID = "tid"
	CtxEmail    = "email"
	CtxRole     = "role"
	CtxIsAdmin  = "is_admin"
)

func RequireAuth(cfg *config.Config) fiber.Handler {
	return func(c *fiber.Ctx) error {
		h := c.Get("Authorization")
		if h == "" {
			return fiber.NewError(fiber.StatusUnauthorized, "missing authorization header")
		}
		parts := strings.SplitN(h, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid authorization header")
		}
		claims, err := auth.Parse(cfg.JWTSecret, parts[1])
		if err != nil {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid or expired token")
		}
		c.Locals(CtxUserID, claims.UserID)
		c.Locals(CtxTenantID, claims.TenantID)
		c.Locals(CtxEmail, claims.Email)
		c.Locals(CtxRole, claims.Role)
		c.Locals(CtxIsAdmin, claims.IsAdmin)
		return c.Next()
	}
}

// RequireRole returns 403 unless the authenticated user has one of the
// allowed roles. Must come AFTER RequireAuth in the chain.
//
//   protected.Post("/team/invites", middleware.RequireRole("owner", "admin"), h.Create)
func RequireRole(allowed ...string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		role := Role(c)
		for _, r := range allowed {
			if r == role {
				return c.Next()
			}
		}
		return fiber.NewError(fiber.StatusForbidden, "insufficient permissions")
	}
}

// TenantID returns the authenticated tenant id, or empty string if unset.
func TenantID(c *fiber.Ctx) string {
	v, _ := c.Locals(CtxTenantID).(string)
	return v
}

// UserID returns the authenticated user id, or empty string if unset.
func UserID(c *fiber.Ctx) string {
	v, _ := c.Locals(CtxUserID).(string)
	return v
}

// Email returns the authenticated user email, or empty string if unset.
func Email(c *fiber.Ctx) string {
	v, _ := c.Locals(CtxEmail).(string)
	return v
}

// Role returns the authenticated user role, or empty string if unset.
func Role(c *fiber.Ctx) string {
	v, _ := c.Locals(CtxRole).(string)
	return v
}

// IsAdmin returns true if the JWT carried the platform-admin flag.
func IsAdmin(c *fiber.Ctx) bool {
	v, _ := c.Locals(CtxIsAdmin).(bool)
	return v
}

// RequireAdmin gates platform-admin endpoints (/api/v1/admin/*).
// Must come AFTER RequireAuth.
func RequireAdmin() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !IsAdmin(c) {
			return fiber.NewError(fiber.StatusForbidden, "platform admin only")
		}
		return c.Next()
	}
}
