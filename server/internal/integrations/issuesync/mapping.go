package issuesync

import (
	"encoding/json"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Status mapping between Multica's fixed vocabulary (backlog, todo,
// in_progress, in_review, done, blocked, cancelled) and each provider's.
//
// GitHub and GitLab issues are binary open/closed. Jira is mapped through
// status CATEGORIES (To Do / In Progress / Done), which survive custom
// workflows; the provider resolves the concrete transition at push time.
//
// issue_sync_source.status_mapping optionally overrides either direction:
//
//	{"inbound": {"closed": "cancelled"}, "outbound": {"blocked": "open"}}
//
// Unmapped inbound states leave the local status untouched; unmapped
// outbound statuses push nothing (status change stays local).

// statusMappingOverrides is the JSONB shape stored on issue_sync_source.
type statusMappingOverrides struct {
	Inbound  map[string]string `json:"inbound,omitempty"`
	Outbound map[string]string `json:"outbound,omitempty"`
}

var defaultInbound = map[string]map[string]string{
	ProviderGitHub: {
		"open":   "todo",
		"closed": "done",
	},
	ProviderGitLab: {
		"opened": "todo",
		"closed": "done",
	},
	// Jira keys are status-category keys ("new" = To Do, "indeterminate" =
	// In Progress, "done" = Done), lowercased by the provider.
	ProviderJira: {
		"new":           "todo",
		"indeterminate": "in_progress",
		"done":          "done",
	},
}

var defaultOutbound = map[string]map[string]string{
	ProviderGitHub: {
		"backlog":     "open",
		"todo":        "open",
		"in_progress": "open",
		"in_review":   "open",
		"blocked":     "open",
		"done":        "closed",
		"cancelled":   "closed",
	},
	ProviderGitLab: {
		"backlog":     "opened",
		"todo":        "opened",
		"in_progress": "opened",
		"in_review":   "opened",
		"blocked":     "opened",
		"done":        "closed",
		"cancelled":   "closed",
	},
	ProviderJira: {
		"backlog":     "new",
		"todo":        "new",
		"in_progress": "indeterminate",
		"in_review":   "indeterminate",
		"blocked":     "indeterminate",
		"done":        "done",
		"cancelled":   "done",
	},
}

func parseOverrides(src db.IssueSyncSource) statusMappingOverrides {
	var o statusMappingOverrides
	if len(src.StatusMapping) > 0 {
		// A malformed override degrades to defaults rather than blocking sync.
		_ = json.Unmarshal(src.StatusMapping, &o)
	}
	return o
}

// MapInboundStatus maps a provider-native state to a Multica status. ok=false
// means "no opinion" — the caller keeps the current local status. An existing
// non-terminal local status is deliberately NOT clobbered by the inbound
// "open" default: an issue being worked on locally (in_progress) stays
// in_progress when the remote side merely remains open. Only transitions that
// cross the open/closed boundary move status by default.
func MapInboundStatus(src db.IssueSyncSource, remoteState, currentStatus string) (string, bool) {
	o := parseOverrides(src)
	mapped, ok := "", false
	if v, hit := o.Inbound[remoteState]; hit {
		mapped, ok = v, true
	} else if v, hit := defaultInbound[src.Provider][remoteState]; hit {
		mapped, ok = v, true
	}
	if !ok || mapped == currentStatus {
		return "", false
	}
	// Keep active local statuses when the remote state maps to the generic
	// "todo" bucket — an open remote issue says nothing about local progress.
	if mapped == "todo" && isActiveStatus(currentStatus) {
		return "", false
	}
	return mapped, true
}

// isActiveStatus reports whether a local status represents in-flight work
// that a generic "remote issue is open" signal must not reset.
func isActiveStatus(s string) bool {
	switch s {
	case "in_progress", "in_review", "blocked":
		return true
	}
	return false
}

// MapOutboundStatus maps a Multica status to the provider's vocabulary.
// ok=false means the status change is not pushed.
func MapOutboundStatus(src db.IssueSyncSource, status string) (string, bool) {
	o := parseOverrides(src)
	if v, hit := o.Outbound[status]; hit {
		return v, true
	}
	if v, hit := defaultOutbound[src.Provider][status]; hit {
		return v, true
	}
	return "", false
}
