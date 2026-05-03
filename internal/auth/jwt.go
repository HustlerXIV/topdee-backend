package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Role names used across the codebase. Keep in sync with the frontend
// Role type and the allow-list in the team handler.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleAgent  = "agent"
	RoleViewer = "viewer"
)

// IsValidRole returns true if the given role is one of the four canonical
// values. Used by team-management endpoints to reject bad input early.
func IsValidRole(r string) bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleAgent, RoleViewer:
		return true
	}
	return false
}

type Claims struct {
	UserID   string `json:"uid"`
	TenantID string `json:"tid"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	// IsAdmin: platform-wide admin flag (Topdee staff). Distinct from
	// `Role` which is the per-tenant role.
	IsAdmin  bool   `json:"is_admin,omitempty"`
	jwt.RegisteredClaims
}

// IssueOpts groups everything that goes into the JWT — a small struct
// because the call sites kept growing positional args.
type IssueOpts struct {
	Secret    string
	UserID    string
	TenantID  string
	Email     string
	Role      string
	IsAdmin   bool
	TTLHours  int
}

func IssueToken(o IssueOpts) (string, error) {
	claims := Claims{
		UserID:   o.UserID,
		TenantID: o.TenantID,
		Email:    o.Email,
		Role:     o.Role,
		IsAdmin:  o.IsAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(o.TTLHours) * time.Hour)),
			Issuer:    "topdee",
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(o.Secret))
}

// Issue is a thin compatibility wrapper for callers that don't want to
// build the IssueOpts struct manually. Defaults IsAdmin to false.
func Issue(secret, userID, tenantID, email, role string, ttlHours int) (string, error) {
	return IssueToken(IssueOpts{
		Secret:   secret,
		UserID:   userID,
		TenantID: tenantID,
		Email:    email,
		Role:     role,
		TTLHours: ttlHours,
	})
}

func Parse(secret, tokenStr string) (*Claims, error) {
	tok, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	c, ok := tok.Claims.(*Claims)
	if !ok || !tok.Valid {
		return nil, errors.New("invalid token")
	}
	return c, nil
}
