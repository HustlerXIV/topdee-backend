package handlers

// Analytics — real-data statistics for the current tenant.
//
//   GET /api/v1/analytics?range=7d|30d|month
//
// All aggregations run against the `messages` collection; dashboard
// (playground) messages are excluded so only real customer conversations
// are counted. Six pipelines run concurrently to keep latency low.

import (
	"context"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/topdee/backend/internal/db"
	"github.com/topdee/backend/internal/middleware"
	"github.com/topdee/backend/internal/models"
)

type AnalyticsHandler struct {
	mongo *db.Mongo
}

func NewAnalyticsHandler(m *db.Mongo) *AnalyticsHandler {
	return &AnalyticsHandler{mongo: m}
}

// ── Response types ────────────────────────────────────────────────────────────

type DailyStat struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type ChannelStat struct {
	Channel string `json:"channel"`
	Count   int    `json:"count"`
	Pct     int    `json:"pct"`
}

type AnalyticsResponse struct {
	// Conversations
	TotalConversations  int `json:"total_conversations"`
	PrevTotalConvs      int `json:"prev_total_conversations"`
	// AI vs human resolution
	AIResolvedCount    int `json:"ai_resolved_count"`
	AIResolvedPct      int `json:"ai_resolved_pct"`
	PrevAIResolvedPct  int `json:"prev_ai_resolved_pct"`
	HumanTakeovers     int `json:"human_takeovers"`
	// Unique customers
	UniqueCustomers     int `json:"unique_customers"`
	PrevUniqueCustomers int `json:"prev_unique_customers"`
	// Breakdowns
	ChannelBreakdown []ChannelStat `json:"channel_breakdown"`
	Daily            []DailyStat   `json:"daily"`
	DaysInRange      int           `json:"days_in_range"`
}

// ── Handler ───────────────────────────────────────────────────────────────────

// GetStats handles GET /api/v1/analytics?range=7d|30d|month
func (h *AnalyticsHandler) GetStats(c *fiber.Ctx) error {
	tid := middleware.TenantID(c)

	// Resolve time window
	rangeStr := c.Query("range", "7d")
	now := time.Now().UTC()
	var start time.Time
	var days int
	switch rangeStr {
	case "30d":
		start = now.AddDate(0, 0, -30)
		days = 30
	case "month":
		y, m, _ := now.Date()
		start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		days = now.Day()
	default: // "7d"
		start = now.AddDate(0, 0, -7)
		days = 7
	}
	prevStart := start.AddDate(0, 0, -days)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Base match fragments reused across pipelines
	currentFilter := bson.M{
		"tenant_id":  tid,
		"channel":    bson.M{"$ne": models.ChannelDashboard},
		"created_at": bson.M{"$gte": start},
	}
	prevFilter := bson.M{
		"tenant_id":  tid,
		"channel":    bson.M{"$ne": models.ChannelDashboard},
		"created_at": bson.M{"$gte": prevStart, "$lt": start},
	}

	// Shared result variables (written only inside their own goroutine)
	var (
		totalConvs          int
		prevConvs           int
		aiResolvedCount     int
		humanTakeovers      int
		prevAIResolved      int
		prevTotalForPct     int
		uniqueCustomers     int
		prevUnique          int
		channelBreakdown    []ChannelStat
		daily               []DailyStat
		wg                  sync.WaitGroup
	)

	col := h.mongo.DB.Collection("messages")

	// ── 1. Total conversations — current period ───────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe := []bson.M{
			{"$match": currentFilter},
			{"$group": bson.M{"_id": "$conversation_id"}},
			{"$count": "n"},
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return
		}
		defer cur.Close(ctx)
		var r []struct {
			N int `bson:"n"`
		}
		_ = cur.All(ctx, &r)
		if len(r) > 0 {
			totalConvs = r[0].N
		}
	}()

	// ── 2. Total conversations — previous period ──────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe := []bson.M{
			{"$match": prevFilter},
			{"$group": bson.M{"_id": "$conversation_id"}},
			{"$count": "n"},
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return
		}
		defer cur.Close(ctx)
		var r []struct {
			N int `bson:"n"`
		}
		_ = cur.All(ctx, &r)
		if len(r) > 0 {
			prevConvs = r[0].N
		}
	}()

	// ── 3. AI resolved vs human takeovers — current period ───────────────────
	// A conversation is "AI resolved" if it had at least one AI reply and zero
	// human replies. A "human takeover" had at least one human reply.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe := []bson.M{
			{"$match": currentFilter},
			{"$group": bson.M{
				"_id": "$conversation_id",
				"has_human": bson.M{"$max": bson.M{"$cond": bson.M{
					"if":   bson.M{"$eq": bson.A{"$role", "human"}},
					"then": 1,
					"else": 0,
				}}},
				"has_ai": bson.M{"$max": bson.M{"$cond": bson.M{
					"if":   bson.M{"$eq": bson.A{"$role", "ai"}},
					"then": 1,
					"else": 0,
				}}},
			}},
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return
		}
		defer cur.Close(ctx)
		var rows []struct {
			HasHuman int `bson:"has_human"`
			HasAI    int `bson:"has_ai"`
		}
		_ = cur.All(ctx, &rows)
		for _, r := range rows {
			if r.HasHuman == 1 {
				humanTakeovers++
			} else if r.HasAI == 1 {
				aiResolvedCount++
			}
		}
	}()

	// ── 4. AI resolved — previous period (for % comparison) ──────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe := []bson.M{
			{"$match": prevFilter},
			{"$group": bson.M{
				"_id": "$conversation_id",
				"has_human": bson.M{"$max": bson.M{"$cond": bson.M{
					"if":   bson.M{"$eq": bson.A{"$role", "human"}},
					"then": 1,
					"else": 0,
				}}},
				"has_ai": bson.M{"$max": bson.M{"$cond": bson.M{
					"if":   bson.M{"$eq": bson.A{"$role", "ai"}},
					"then": 1,
					"else": 0,
				}}},
			}},
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return
		}
		defer cur.Close(ctx)
		var rows []struct {
			HasHuman int `bson:"has_human"`
			HasAI    int `bson:"has_ai"`
		}
		_ = cur.All(ctx, &rows)
		localAI := 0
		for _, r := range rows {
			prevTotalForPct++
			if r.HasHuman == 0 && r.HasAI == 1 {
				localAI++
			}
		}
		prevAIResolved = localAI
	}()

	// ── 5. Unique customers — current period ──────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe := []bson.M{
			{"$match": currentFilter},
			{"$match": bson.M{"external_user_id": bson.M{"$ne": ""}}},
			{"$group": bson.M{"_id": "$external_user_id"}},
			{"$count": "n"},
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return
		}
		defer cur.Close(ctx)
		var r []struct {
			N int `bson:"n"`
		}
		_ = cur.All(ctx, &r)
		if len(r) > 0 {
			uniqueCustomers = r[0].N
		}
	}()

	// ── 6. Unique customers — previous period ─────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe := []bson.M{
			{"$match": prevFilter},
			{"$match": bson.M{"external_user_id": bson.M{"$ne": ""}}},
			{"$group": bson.M{"_id": "$external_user_id"}},
			{"$count": "n"},
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return
		}
		defer cur.Close(ctx)
		var r []struct {
			N int `bson:"n"`
		}
		_ = cur.All(ctx, &r)
		if len(r) > 0 {
			prevUnique = r[0].N
		}
	}()

	// ── 7. Channel breakdown ──────────────────────────────────────────────────
	// Group conversations (not raw messages) so a long conversation doesn't
	// inflate LINE's share over Facebook.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe := []bson.M{
			{"$match": currentFilter},
			// Collapse to one row per conversation, keep its channel.
			{"$group": bson.M{
				"_id":     "$conversation_id",
				"channel": bson.M{"$first": "$channel"},
			}},
			// Count conversations per channel.
			{"$group": bson.M{
				"_id":   "$channel",
				"count": bson.M{"$sum": 1},
			}},
			{"$sort": bson.M{"count": -1}},
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return
		}
		defer cur.Close(ctx)
		var rows []struct {
			Channel string `bson:"_id"`
			Count   int    `bson:"count"`
		}
		_ = cur.All(ctx, &rows)
		total := 0
		for _, r := range rows {
			total += r.Count
		}
		for _, r := range rows {
			pct := 0
			if total > 0 {
				pct = r.Count * 100 / total
			}
			channelBreakdown = append(channelBreakdown, ChannelStat{
				Channel: r.Channel,
				Count:   r.Count,
				Pct:     pct,
			})
		}
	}()

	// ── 8. Daily conversations ────────────────────────────────────────────────
	// Each conversation is attributed to the day its first message arrived.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe := []bson.M{
			{"$match": currentFilter},
			// Earliest message timestamp per conversation.
			{"$group": bson.M{
				"_id":  "$conversation_id",
				"date": bson.M{"$min": "$created_at"},
			}},
			// Convert to YYYY-MM-DD string.
			{"$project": bson.M{
				"date": bson.M{"$dateToString": bson.M{
					"format": "%Y-%m-%d",
					"date":   "$date",
				}},
			}},
			// Count conversations per day.
			{"$group": bson.M{
				"_id":   "$date",
				"count": bson.M{"$sum": 1},
			}},
			{"$sort": bson.M{"_id": 1}},
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return
		}
		defer cur.Close(ctx)
		var rows []struct {
			Date  string `bson:"_id"`
			Count int    `bson:"count"`
		}
		_ = cur.All(ctx, &rows)
		for _, r := range rows {
			daily = append(daily, DailyStat{Date: r.Date, Count: r.Count})
		}
	}()

	wg.Wait()

	// Compute derived percentages
	aiResolvedPct := 0
	if totalConvs > 0 {
		aiResolvedPct = aiResolvedCount * 100 / totalConvs
	}
	prevAIResolvedPct := 0
	if prevTotalForPct > 0 {
		prevAIResolvedPct = prevAIResolved * 100 / prevTotalForPct
	}

	// Ensure nil slices become empty arrays in JSON
	if channelBreakdown == nil {
		channelBreakdown = []ChannelStat{}
	}
	if daily == nil {
		daily = []DailyStat{}
	}

	return c.JSON(AnalyticsResponse{
		TotalConversations:  totalConvs,
		PrevTotalConvs:      prevConvs,
		AIResolvedCount:     aiResolvedCount,
		AIResolvedPct:       aiResolvedPct,
		PrevAIResolvedPct:   prevAIResolvedPct,
		HumanTakeovers:      humanTakeovers,
		UniqueCustomers:     uniqueCustomers,
		PrevUniqueCustomers: prevUnique,
		ChannelBreakdown:    channelBreakdown,
		Daily:               daily,
		DaysInRange:         days,
	})
}
