package models

import "time"

// Conversation holds per-conversation state that isn't derivable from the
// messages collection alone.  Today that means the human-handoff flag; more
// fields (tags, assigned agent, …) can be added here without touching the
// hot messages collection.
//
// The collection is keyed on _id = conversation_id (same string the webhook
// router writes into every message).  Documents are upserted lazily — we only
// create one when something interesting happens (handoff request, tag, close).
type Conversation struct {
	ID            string     `bson:"_id"              json:"id"`
	TenantID      string     `bson:"tenant_id"        json:"tenant_id"`
	NeedsHuman    bool       `bson:"needs_human"      json:"needs_human"`
	NeedsHumanAt  *time.Time `bson:"needs_human_at,omitempty" json:"needs_human_at,omitempty"`
	ResolvedAt    *time.Time `bson:"resolved_at,omitempty"    json:"resolved_at,omitempty"`
	CreatedAt     time.Time  `bson:"created_at"       json:"created_at"`
	UpdatedAt     time.Time  `bson:"updated_at"       json:"updated_at"`
}
