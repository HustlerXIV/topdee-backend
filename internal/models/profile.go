package models

import "time"

// CustomerProfile — cached display name + avatar for one external customer
// (LINE / Facebook / etc.) so the inbox doesn't have to call the platform
// profile API on every list refresh.
//
// Keyed by (provider, external_user_id) — globally unique per platform,
// not per-tenant, since the same LINE user may chat with multiple tenants
// who each have a connection to that user's pool.
//
// Refreshed lazily: when the inbox aggregation finds no profile for a
// user, the webhook handler is asked to backfill on the next inbound
// message from that user.
type CustomerProfile struct {
	ID             string    `bson:"_id" json:"id"`
	Provider       string    `bson:"provider" json:"provider"`
	ExternalUserID string    `bson:"external_user_id" json:"external_user_id"`
	DisplayName    string    `bson:"display_name" json:"display_name"`
	PictureURL     string    `bson:"picture_url,omitempty" json:"picture_url,omitempty"`
	Language       string    `bson:"language,omitempty" json:"language,omitempty"`
	UpdatedAt      time.Time `bson:"updated_at" json:"updated_at"`
}
