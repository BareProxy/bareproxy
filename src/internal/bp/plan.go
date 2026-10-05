// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

// PlanResult says what replacing one config with another would change.
// STUB: the real analysis is being written; the API below is fixed.
type PlanResult struct {
	ID       string     // short hash of both configs; apply --plan checks it
	OldVer   int        // running version the plan was made against (0 = a file)
	Changes  []PlanLine // classes of requests handled differently
	Settings []string   // other changes: listeners, pools, certificates, globals
	Warnings []string   // rules that win nothing, pools no reachable rule uses
	TooMany  bool       // too many classes: Changes lists changed rules instead
}

// PlanLine is one class of requests whose handling changes.
type PlanLine struct {
	Where string // e.g. "example.com (port 443)"
	What  string // e.g. "any method, /api/v2 and below"
	Old   string // e.g. "pool api, strip /api"
	New   string // e.g. "pool api-v2, strip /api/v2"
}

// MakePlan compares two compiled configs. Neither is changed.
func MakePlan(old, new *Config) *PlanResult {
	return &PlanResult{ID: PlanID(old, new)}
}

// PlanID is a short hash of both configs' text.
func PlanID(old, new *Config) string { return "000000000000" }

// Empty reports whether the plan changes nothing at all.
func (p *PlanResult) Empty() bool { return len(p.Changes) == 0 && len(p.Settings) == 0 }

// Text renders the plan for a terminal.
func (p *PlanResult) Text() string { return "No changes.\n" }
