package models

import "time"

// KnowledgeBase is a tenant-owned collection of files that the platform agent
// retrieves from when answering messages. It replaces the per-tenant Agent
// model — Shape 2 has one platform-owned agent and many tenant knowledge bases.
type KnowledgeBase struct {
	ID          string          `bson:"_id" json:"id"`
	TenantID    string          `bson:"tenant_id" json:"tenant_id"`
	Name        string          `bson:"name" json:"name"`
	Description string          `bson:"description" json:"description"`
	Files       []KnowledgeFile `bson:"files" json:"files"`
	ChunkCount  int             `bson:"chunk_count" json:"chunk_count"`
	CreatedAt   time.Time       `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time       `bson:"updated_at" json:"updated_at"`
}

type KnowledgeFile struct {
	Filename   string    `bson:"filename" json:"filename"`
	Size       int       `bson:"size" json:"size"`
	Chunks     int       `bson:"chunks" json:"chunks"`
	UploadedAt time.Time `bson:"uploaded_at" json:"uploaded_at"`
}
