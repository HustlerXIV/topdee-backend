package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/topdee/backend/internal/clients"
	"github.com/topdee/backend/internal/db"
)

func Health(m *db.Mongo, ai *clients.AIClient) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
		defer cancel()

		mongoOK := m.Client.Ping(ctx, nil) == nil
		aiOK := ai.Health(ctx) == nil

		status := "ok"
		if !mongoOK || !aiOK {
			status = "degraded"
		}
		return c.JSON(fiber.Map{
			"status": status,
			"deps": fiber.Map{
				"mongo":      mongoOK,
				"ai_service": aiOK,
			},
		})
	}
}
