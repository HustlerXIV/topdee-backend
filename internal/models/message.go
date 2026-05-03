package models

import "time"

const (
	RoleUser  = "user"
	RoleAI    = "ai"
	RoleHuman = "human"

	ChannelDashboard = "dashboard"
	ChannelFacebook  = "facebook"
	ChannelLine      = "line"
)

// Message — one turn in a conversation. Conversations are scoped to a tenant
// and a channel. There's no agent_id because Shape 2 has one platform agent.
type Message struct {
	ID             string         `bson:"_id" json:"id"`
	TenantID       string         `bson:"tenant_id" json:"tenant_id"`
	ConversationID string         `bson:"conversation_id" json:"conversation_id"`
	Role           string         `bson:"role" json:"role"`     // user | ai | human
	Content        string         `bson:"content" json:"content"`
	Channel        string         `bson:"channel" json:"channel"` // dashboard | facebook | line
	ExternalUserID string         `bson:"external_user_id,omitempty" json:"external_user_id,omitempty"`
	Metadata       map[string]any `bson:"metadata,omitempty" json:"metadata,omitempty"`
	CreatedAt      time.Time      `bson:"created_at" json:"created_at"`
}
