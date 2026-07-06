// Package issuesync implements bidirectional issue synchronization between
// Multica issues and external trackers (GitHub Issues, GitLab issues, Jira
// Cloud).
//
// Topology: a Multica project attaches N issue_sync_source rows, each binding
// one remote container (repo / GitLab project / Jira project) through one
// workspace-level provider connection. Inbound flow: provider webhook →
// handler normalizes the payload into an IssueEvent → Engine.ApplyRemote
// upserts the local issue/comment with ActorType "issue_sync". Outbound flow:
// event-bus listeners (cmd/server/issuesync_listeners.go) enqueue
// issue_sync_outbox rows for non-sync actors → the outbox worker pushes to
// the provider and records the pushed content hash for echo suppression.
//
// Echo suppression is layered:
//  1. Bus events with ActorType "issue_sync" are ignored by the outbound
//     listeners, so inbound applies never re-enqueue.
//  2. Webhook deliveries caused by our own remote writes are dropped when the
//     normalized content hash equals external_issue_link.last_pushed_hash, or
//     when the webhook actor is the connection's own identity (checked by the
//     provider-specific webhook handlers, which have the connection at hand).
//  3. Inbound events older than external_issue_link.remote_updated_at are
//     stale replays and are dropped (last-write-wins clock).
package issuesync

import (
	"context"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Provider name constants — match the issue_sync_source.provider CHECK.
const (
	ProviderGitHub = "github"
	ProviderGitLab = "gitlab"
	ProviderJira   = "jira"
)

// ExternalUser is the provider-side identity attached to issues, comments,
// and webhook actors. AccountID is the provider-stable identifier (GitHub
// numeric user id, GitLab numeric user id, Jira accountId).
type ExternalUser struct {
	AccountID   string
	Login       string
	DisplayName string
	Email       string
	AvatarURL   string
}

// ExternalIssue is the provider-neutral shape of a remote issue.
// Description is markdown (providers convert from their native format, e.g.
// Jira ADF). State carries the provider-native status vocabulary; mapping to
// Multica statuses happens in mapping.go using the source's overrides.
type ExternalIssue struct {
	// ID is the provider-stable identifier used for link identity (GitHub
	// numeric issue id, GitLab global issue id, Jira issue id).
	ID string
	// Key is the human handle: "#123" for GitHub/GitLab, "PROJ-42" for Jira.
	Key         string
	Title       string
	Description string
	State       string
	Labels      []string
	Assignee    *ExternalUser
	Author      *ExternalUser
	WebURL      string
	// ParentExternalID is set when this issue is a subtask. It carries the
	// provider-stable ID of the parent issue so the engine can link the
	// local issue's parent_issue_id. Empty for top-level issues.
	ParentExternalID string
	UpdatedAt        time.Time
}

// ExternalComment is the provider-neutral shape of a remote comment. Body is
// markdown.
type ExternalComment struct {
	ID        string
	Body      string
	Author    *ExternalUser
	WebURL    string
	UpdatedAt time.Time
}

// OutboundIssue is the content pushed to a provider when a Multica issue
// changes. State is already provider vocabulary (mapped before dispatch);
// AssigneeAccountID is empty when unassigned or unmappable.
type OutboundIssue struct {
	Title             string
	Description       string
	State             string
	Labels            []string
	AssigneeAccountID string
}

// Container is one attachable remote repo/project, listed for the frontend
// picker. Key is the normalized external_key stored on issue_sync_source.
type Container struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// Ref is the JSONB external_ref payload to store when attaching.
	Ref map[string]any `json:"ref"`
}

// Provider abstracts one external tracker. Implementations resolve their own
// credentials from the source's connection_id (installation token for GitHub,
// stored OAuth token for GitLab/Jira) and convert between provider payloads
// and the neutral types above.
type Provider interface {
	Name() string

	// ListContainers lists remote repos/projects reachable through the given
	// workspace connection, for the attach picker.
	ListContainers(ctx context.Context, connectionID string) ([]Container, error)

	// ListIssues pages through the container's issues for backfill. cursor is
	// provider-opaque ("" starts from the beginning); the returned cursor is
	// "" when exhausted.
	ListIssues(ctx context.Context, src db.IssueSyncSource, cursor string) ([]ExternalIssue, string, error)

	// CreateIssue creates a remote issue and returns its identity.
	CreateIssue(ctx context.Context, src db.IssueSyncSource, out OutboundIssue) (*ExternalIssue, error)

	// UpdateIssue patches title/description/state/labels/assignee of an
	// existing remote issue.
	UpdateIssue(ctx context.Context, src db.IssueSyncSource, externalID string, out OutboundIssue) (*ExternalIssue, error)

	// CreateComment mirrors a local comment to the remote issue.
	CreateComment(ctx context.Context, src db.IssueSyncSource, externalID, body string) (*ExternalComment, error)

	// UpdateComment propagates a local comment edit.
	UpdateComment(ctx context.Context, src db.IssueSyncSource, externalID, commentID, body string) (*ExternalComment, error)
}

// IssueEvent is a normalized inbound webhook event, produced by the
// provider-specific webhook handlers and consumed by Engine.ApplyRemote.
type IssueEvent struct {
	// Kind is one of "issue" (created or updated — apply is an upsert) or
	// "comment" (created or updated).
	Kind    string
	Issue   ExternalIssue
	Comment *ExternalComment
}
