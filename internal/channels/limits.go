package channels

// Plan-based caps on how many connections of each provider a tenant may
// create. Defined as a static map so changing limits is a one-line edit;
// long-term this should move to env or a `plans` collection.
//
// The "default" entry is the fallback when a tenant's `plan` field doesn't
// match any known plan name (treat as the most conservative tier).

type PlanLimit struct {
	Facebook int
	Line     int
}

var planLimits = map[string]PlanLimit{
	"free":       {Facebook: 1, Line: 1},
	"starter":    {Facebook: 1, Line: 1},
	"basic":      {Facebook: 3, Line: 1},
	"growth":     {Facebook: 5, Line: 3},
	"pro":        {Facebook: 10, Line: 5},
	"enterprise": {Facebook: 100, Line: 100},
	"default":    {Facebook: 1, Line: 1},
}

// LimitFor returns how many `provider` connections plan `plan` allows.
// Unknown plans fall back to the default tier; unknown providers return 0
// (effectively forbidding the connection).
func LimitFor(plan, provider string) int {
	pl, ok := planLimits[plan]
	if !ok {
		pl = planLimits["default"]
	}
	switch provider {
	case "facebook":
		return pl.Facebook
	case "line":
		return pl.Line
	}
	return 0
}

// LimitsForPlan returns the full table for one plan, useful for UI hints
// like "your plan allows N Facebook pages".
func LimitsForPlan(plan string) PlanLimit {
	pl, ok := planLimits[plan]
	if !ok {
		return planLimits["default"]
	}
	return pl
}
