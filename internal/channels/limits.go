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
		ChannelLimitMode string         `bson:"channel_limit_mode"`
		TotalChannels    int            `bson:"total_channels"`
		Channels         map[string]int `bson:"channels"`
		Members          int            `bson:"members"`
		MessagesPerMonth int            `bson:"messages_per_month"`
		KnowledgeBases   int            `bson:"knowledge_bases"`
		StorageMB        int            `bson:"storage_mb"`
	} `bson:"limits"`
}

// ChannelPolicy captures the channel-capping rules for one plan. Built once
// per request (one DB roundtrip) and passed to handlers so per-provider and
// total-cap checks share the same view of the plan.
type ChannelPolicy struct {
	// Mode is either "per_provider" (the default) or "total".
	Mode string
	// Total is the cap on the sum of all provider connections — only
	// meaningful when Mode == "total". -1 means unlimited.
	Total int
	// ProviderCaps is the per-provider map exactly as stored on the plan.
	// In total-cap mode it's still used as a visibility gate (cap == 0
	// hides a provider entirely).
	ProviderCaps map[string]int
}

// IsTotalMode is a tiny helper so call sites read naturally.
func (p ChannelPolicy) IsTotalMode() bool { return p.Mode == "total" }

// ProviderHidden returns true when a provider has an explicit cap of 0 on
// this plan, regardless of mode — the admin set it that way to hide the
// provider from this tier.
func (p ChannelPolicy) ProviderHidden(provider string) bool {
	v, ok := p.ProviderCaps[provider]
	return ok && v == 0
}

// PolicyForCtx loads the channel policy for a plan in a single DB roundtrip.
// Falls back to per-provider mode with hardcoded defaults when the plans
// collection is unavailable or the plan doesn't exist — same shape as the
// legacy LimitForCtx behavior.
func PolicyForCtx(ctx context.Context, db *mongo.Database, plan string) ChannelPolicy {
	if db != nil {
		tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var doc planDoc
		if err := db.Collection("plans").FindOne(tctx, bson.M{"_id": plan}).Decode(&doc); err == nil {
			mode := doc.Limits.ChannelLimitMode
			if mode == "" {
				mode = "per_provider"
			}
			caps := doc.Limits.Channels
			if caps == nil {
				caps = map[string]int{}
			}
			return ChannelPolicy{
				Mode:         mode,
				Total:        doc.Limits.TotalChannels,
				ProviderCaps: caps,
			}
		}
	}
	// Bootstrap fallback — synthesize a per-provider policy from the
	// hardcoded table so the system stays usable without a seeded plans
	// collection.
	pl := LimitsForPlan(plan)
	return ChannelPolicy{
		Mode: "per_provider",
		ProviderCaps: map[string]int{
			"facebook":  pl.Facebook,
			"instagram": pl.Instagram,
			"line":      pl.Line,
			"web":       pl.Web,
		},
	}
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
