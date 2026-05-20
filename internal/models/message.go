package models

import "time"

const (
	RoleUser  = "user"
	RoleAI    = "ai"
	RoleHuman = "human"
	// RoleSuggestion is an AI-generated draft for the team. It is stored in
	// the inbox but is not sent to the customer until a human sends it.
	RoleSuggestion = "suggestion"

	ChannelDashboard = "dashboard"
	ChannelFacebook  = "facebook"
	ChannelLine      = "line"
	ChannelWeb       = "web"
)

// Message — one turn in a conversation. Conversations are scoped to a tenant
// and a channel. There's no agent_id because Shape 2 has one platform agent.
type Message struct {
	ID             string         `bson:"_id" json:"id"`
	TenantID       string         `bson:"tenant_id" json:"tenant_id"`
	ConversationID string         `bson:"conversation_id" json:"conversation_id"`
	Role           string         `bson:"role" json:"role"` // user | ai | human | suggestion
	Content        string         `bson:"content" json:"content"`
	Channel        string         `bson:"channel" json:"channel"` // dashboard | facebook | line
	ExternalUserID string         `bson:"external_user_id,omitempty" json:"external_user_id,omitempty"`
	// SenderName is the display name of the team member who sent this message
	// (role=human). Empty for AI, user, and suggestion messages.
	SenderName  string         `bson:"sender_name,omitempty" json:"sender_name,omitempty"`
	Attachments []Attachment   `bson:"attachments,omitempty" json:"attachments,omitempty"`
	Metadata    map[string]any `bson:"metadata,omitempty" json:"metadata,omitempty"`
	CreatedAt   time.Time      `bson:"created_at" json:"created_at"`
}

type Attachment struct {
	ID          string `bson:"id,omitempty" json:"id,omitempty"`
	Type        string `bson:"type" json:"type"` // image | video | audio | file
	URL         string `bson:"url,omitempty" json:"url,omitempty"`
	ContentType string `bson:"content_type,omitempty" json:"content_type,omitempty"`
	Name        string `bson:"name,omitempty" json:"name,omitempty"`
}
