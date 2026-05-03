package handlers

// Per-tenant weekly business hours. Stored as a sub-document on the tenant.
// The orchestrator (chat.go) reads this on every chat turn so the AI can
// tell customers when the shop is open / closed.
//
// Indexing convention: Days[0] = Sunday, Days[6] = Saturday — matches
// JavaScript's Date.getDay() and Go's time.Weekday so the frontend & backend
// can swap rows trivially.

import (
	"regexp"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type BusinessHoursHandler struct {
	mongo *db.Mongo
}

func NewBusinessHoursHandler(m *db.Mongo) *BusinessHoursHandler {
	return &BusinessHoursHandler{mongo: m}
}

// HH:MM, 24h. Permits 0:00 – 23:59. Bare validation only; the UI's <input
// type="time"> already keeps users on the rails, this is just a guardrail.
var timeOfDayRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// defaults — Mon–Fri 09:00–18:00, Sat 10:00–17:00, Sun closed.
// Returned by GET when the tenant hasn't saved anything yet so the UI can
// render straight away.
func defaultBusinessHours() models.BusinessHours {
	return models.BusinessHours{
		Timezone:          "Asia/Bangkok",
		OutOfHoursMessage: "ขณะนี้อยู่นอกเวลาทำการ ทีมงานจะติดต่อกลับโดยเร็วที่สุดค่ะ 🙏",
		Days: [7]models.DayHours{
			{Enabled: false, Open: "10:00", Close: "17:00"}, // Sun
			{Enabled: true, Open: "09:00", Close: "18:00"},  // Mon
			{Enabled: true, Open: "09:00", Close: "18:00"},  // Tue
			{Enabled: true, Open: "09:00", Close: "18:00"},  // Wed
			{Enabled: true, Open: "09:00", Close: "18:00"},  // Thu
			{Enabled: true, Open: "09:00", Close: "18:00"},  // Fri
			{Enabled: true, Open: "10:00", Close: "17:00"},  // Sat
		},
	}
}

// GET /api/v1/business-hours — always returns a populated schedule.
func (h *BusinessHoursHandler) Get(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	var t models.Tenant
	if err := h.mongo.DB.Collection("tenants").
		FindOne(c.Context(), bson.M{"_id": tid}).Decode(&t); err != nil {
		return err
	}
	if t.BusinessHours == nil {
		def := defaultBusinessHours()
		return c.JSON(def)
	}
	return c.JSON(*t.BusinessHours)
}

type bhUpdateReq struct {
	Timezone          string                `json:"timezone"`
	OutOfHoursMessage string                `json:"out_of_hours_message"`
	Days              []models.DayHours     `json:"days"`
}

// PUT /api/v1/business-hours
func (h *BusinessHoursHandler) Update(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)
	var req bhUpdateReq
	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if len(req.Days) != 7 {
		return fiber.NewError(fiber.StatusBadRequest, "days must contain exactly 7 entries (Sun..Sat)")
	}
	if req.Timezone == "" {
		req.Timezone = "Asia/Bangkok"
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid IANA timezone")
	}
	if len(req.OutOfHoursMessage) > 1000 {
		return fiber.NewError(fiber.StatusBadRequest, "out_of_hours_message too long")
	}

	var days [7]models.DayHours
	for i, d := range req.Days {
		// Validate HH:MM only when the day is enabled (closed days don't need
		// usable times — the UI may leave defaults visible).
		if d.Enabled {
			if !timeOfDayRe.MatchString(d.Open) || !timeOfDayRe.MatchString(d.Close) {
				return fiber.NewError(fiber.StatusBadRequest, "open/close must be HH:MM")
			}
			if d.Open >= d.Close {
				return fiber.NewError(fiber.StatusBadRequest, "open must be earlier than close")
			}
		}
		days[i] = d
	}

	bh := models.BusinessHours{
		Timezone:          req.Timezone,
		OutOfHoursMessage: req.OutOfHoursMessage,
		Days:              days,
		UpdatedAt:         time.Now().UTC(),
	}
	_, err := h.mongo.DB.Collection("tenants").UpdateOne(
		c.Context(),
		bson.M{"_id": tid},
		bson.M{"$set": bson.M{"business_hours": bh}},
	)
	if err != nil {
		return err
	}
	return c.JSON(bh)
}
