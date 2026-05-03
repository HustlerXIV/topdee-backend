package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"
)

func ErrorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	msg := "internal server error"

	var fe *fiber.Error
	if errors.As(err, &fe) {
		code = fe.Code
		msg = fe.Message
	} else if err != nil {
		msg = err.Error()
	}
	return c.Status(code).JSON(fiber.Map{"error": msg})
}
