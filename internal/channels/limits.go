package channels

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// PlanLimit is kept for backward-compat with callers that use LimitsForPlan.
type PlanLimit struct {
	Facebook  int
	Instagram int
	Line      int
	Web       int
}

// planDoc is a minimal projection of the plans collection used by LimitForCtx.
type planDoc struct {
	Limits struct {
		Channels         map[string]int `bson:"channels"`
		Members          int            `bson:"members"`
		MessagesPerMonth int            `bson:"messages_per_month"`
		KnowledgeBases   int            `bson:"knowledge_bases"`
		StorageMB        int            `bson:"storage_mb"`
	} `bson:"limits"`
}

// LimitForCtx returns how many `provider` connections `plan` allows by
// reading the plans collection from MongoDB. Falls back to hardcoded defaults
// when the collection is unavailable or the plan doesn't exist.
func LimitForCtx(ctx context.Context, db *mongo.Database, plan, provider string) int {
	if db != nil {
		tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var doc planDoc
		if err := db.Collection("plans").FindOne(tctx, bson.M{"_id": plan}).Decode(&doc); err == nil {
			if v, ok := doc.Limits.Channels[provider]; ok {
				return v
			}
			// Provider key missing in the plan document — fall back to the
			// hardcoded table so newly-added providers (e.g. "web") don't
			// need a DB migration on every existing plan.
			return limitFallback(plan, provider)
		}
	}
	// Fallback to hardcoded defaults (legacy / bootstrap).
	return limitFallback(plan, provider)
}

// MemberLimitForCtx returns the max team members allowed for a plan.
func MemberLimitForCtx(ctx context.Context, db *mongo.Database, plan string) int {
	if db != nil {
		tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var doc planDoc
		if err := db.Collection("plans").FindOne(tctx, bson.M{"_id": plan}).Decode(&doc); err == nil {
			return doc.Limits.Members
		}
	}
	return 5 // hardcoded fallback
}

// LimitFor is the original signature kept for callers without a DB reference.
// Uses hardcoded defaults only.
func LimitFor(plan, provider string) int {
	return limitFallback(plan, provider)
}

func limitFallback(plan, provider string) int {
	pl, ok := hardcodedLimits[plan]
	if !ok {
		pl = hardcodedLimits["default"]
	}
	switch provider {
	case "facebook":
		return pl.Facebook
	case "instagram":
		return pl.Instagram
	case "line":
		return pl.Line
	case "web":
		return pl.Web
	}
	return 0
}

// hardcodedLimits are bootstrap values used before the plans collection is
// seeded, and as the ultimate fallback.
var hardcodedLimits = map[string]PlanLimit{
	"free":       {Facebook: 1, Instagram: 1, Line: 1, Web: 1},
	"starter":    {Facebook: 1, Instagram: 1, Line: 1, Web: 1},
	"basic":      {Facebook: 3, Instagram: 3, Line: 1, Web: 3},
	"growth":     {Facebook: 5, Instagram: 5, Line: 3, Web: 5},
	"pro":        {Facebook: 10, Instagram: 10, Line: 5, Web: 10},
	"enterprise": {Facebook: 100, Instagram: 100, Line: 100, Web: 100},
	"default":    {Facebook: 1, Instagram: 1, Line: 1, Web: 1},
}

// LimitsForPlan returns the full hardcoded table for one plan.
func LimitsForPlan(plan string) PlanLimit {
	pl, ok := hardcodedLimits[plan]
	if !ok {
		return hardcodedLimits["default"]
	}
	return pl
}
