package handlers

// Repurposed in the Shape 2 refactor: this file now hosts the knowledge-base
// handlers. The old per-tenant agent CRUD has been removed — Shape 2 uses one
// platform-owned agent (config-driven) and many tenant knowledge bases.

import (
	"context"
	"io"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/topdee/backend/internal/channels"
	"github.com/topdee/backend/internal/clients"
	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type KnowledgeHandler struct {
	mongo *db.Mongo
	ai    *clients.AIClient
}

func NewKnowledgeHandler(m *db.Mongo, ai *clients.AIClient) *KnowledgeHandler {
	return &KnowledgeHandler{mongo: m, ai: ai}
}

func (h *KnowledgeHandler) col() *mongo.Collection {
	return h.mongo.DB.Collection("knowledge_bases")
}

// List returns the tenant's knowledge bases (without file lists).
func (h *KnowledgeHandler) List(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	cur, err := h.col().Find(c.Context(), bson.M{"tenant_id": tid})
	if err != nil {
		return err
	}
	var out []models.KnowledgeBase
	if err := cur.All(c.Context(), &out); err != nil {
		return err
	}
	if out == nil {
		out = []models.KnowledgeBase{}
	}
	return c.JSON(out)
}

type kbInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *KnowledgeHandler) Create(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	var in kbInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if in.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name required")
	}

	// Enforce the plan's knowledge-base count limit. -1 = unlimited; a value
	// of 0 (unset/legacy plan doc) is treated as unlimited so we never block
	// on a config gap. Only a positive cap is enforced.
	plan := tenantPlan(c.Context(), h.mongo, tid)
	if kbLimit := channels.KnowledgeBaseLimitForCtx(c.Context(), h.mongo.DB, plan); kbLimit > 0 {
		existing, err := h.col().CountDocuments(c.Context(), bson.M{"tenant_id": tid})
		if err != nil {
			return err
		}
		if existing >= int64(kbLimit) {
			return fiber.NewError(fiber.StatusForbidden,
				"knowledge base limit reached for your plan — upgrade to add more")
		}
	}

	now := time.Now().UTC()
	kb := models.KnowledgeBase{
		ID:          uuid.NewString(),
		TenantID:    tid,
		Name:        in.Name,
		Description: in.Description,
		Files:       []models.KnowledgeFile{},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if _, err := h.col().InsertOne(c.Context(), kb); err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(kb)
}

func (h *KnowledgeHandler) Get(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	id := c.Params("id")
	var kb models.KnowledgeBase
	err := h.col().FindOne(c.Context(), bson.M{"_id": id, "tenant_id": tid}).Decode(&kb)
	if err == mongo.ErrNoDocuments {
		return fiber.NewError(fiber.StatusNotFound, "knowledge base not found")
	}
	if err != nil {
		return err
	}
	return c.JSON(kb)
}

func (h *KnowledgeHandler) Delete(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	id := c.Params("id")

	// Drop vectors first; if Qdrant fails we'd rather leave the Mongo doc
	// in place so the user can retry than orphan vectors silently.
	if err := h.ai.DeleteKnowledgeBase(c.Context(), tid, id); err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "ai service: "+err.Error())
	}
	res, err := h.col().DeleteOne(c.Context(), bson.M{"_id": id, "tenant_id": tid})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return fiber.NewError(fiber.StatusNotFound, "knowledge base not found")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// tenantStorageBytes sums the size of every uploaded file across all of a
// tenant's knowledge bases. Used to enforce the plan storage quota.
func (h *KnowledgeHandler) tenantStorageBytes(ctx context.Context, tid string) (int64, error) {
	cur, err := h.col().Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"tenant_id": tid}}},
		{{Key: "$unwind", Value: "$files"}},
		{{Key: "$group", Value: bson.M{"_id": nil, "total": bson.M{"$sum": "$files.size"}}}},
	})
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)
	var res []struct {
		Total int64 `bson:"total"`
	}
	if err := cur.All(ctx, &res); err != nil {
		return 0, err
	}
	if len(res) > 0 {
		return res[0].Total, nil
	}
	return 0, nil
}

// UploadFile accepts a multipart upload, forwards to the AI service for
// chunking/embedding, and records file metadata + chunk count on the KB.
func (h *KnowledgeHandler) UploadFile(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	kbID := c.Params("id")

	// Ensure KB exists and belongs to this tenant.
	count, err := h.col().CountDocuments(c.Context(), bson.M{"_id": kbID, "tenant_id": tid})
	if err != nil {
		return err
	}
	if count == 0 {
		return fiber.NewError(fiber.StatusNotFound, "knowledge base not found")
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "missing 'file' field")
	}

	// Enforce the plan's total storage quota before reading the file into
	// memory. -1/0 = unlimited (config gap or unlimited tier); only a positive
	// StorageMB is enforced.
	plan := tenantPlan(c.Context(), h.mongo, tid)
	if storageMB := channels.StorageMBLimitForCtx(c.Context(), h.mongo.DB, plan); storageMB > 0 {
		limitBytes := int64(storageMB) * 1024 * 1024
		used, err := h.tenantStorageBytes(c.Context(), tid)
		if err != nil {
			return err
		}
		if used+fileHeader.Size > limitBytes {
			return fiber.NewError(fiber.StatusForbidden,
				"storage limit reached for your plan — upgrade or remove files to free space")
		}
	}

	src, err := fileHeader.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	content, err := io.ReadAll(src)
	if err != nil {
		return err
	}

	resp, err := h.ai.IngestFile(c.Context(), tid, kbID, fileHeader.Filename, content)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "ai service: "+err.Error())
	}

	now := time.Now().UTC()
	file := models.KnowledgeFile{
		Filename:   fileHeader.Filename,
		Size:       int(fileHeader.Size),
		Chunks:     resp.Chunks,
		UploadedAt: now,
	}
	_, err = h.col().UpdateOne(c.Context(),
		bson.M{"_id": kbID, "tenant_id": tid},
		bson.M{
			"$push": bson.M{"files": file},
			"$inc":  bson.M{"chunk_count": resp.Chunks},
			"$set":  bson.M{"updated_at": now},
		},
	)
	if err != nil {
		return err
	}

	return h.Get(c)
}
