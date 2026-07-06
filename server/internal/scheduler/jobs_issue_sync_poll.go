package scheduler

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/issuesync"
)

// JobNameIssueSyncPoll is the canonical name for the issue-sync polling job.
const JobNameIssueSyncPoll = "issue_sync_poll"

// IssueSyncPollJob returns a JobSpec that polls every enabled sync source every
// 10 minutes, pulling recent issue changes from the remote tracker. This is the
// webhook fallback: when a provider's webhook delivery is absent (not
// configured, expired, or firewalled), the poll ensures changes still land.
//
// ApplyRemote is idempotent — unchanged issues are dropped by the stale-replay
// clock and content-hash echo-suppression, so a poll that finds nothing new is
// a cheap no-op (one API page per source).
func IssueSyncPollJob(engine *issuesync.Engine) JobSpec {
	return JobSpec{
		Name:              JobNameIssueSyncPoll,
		Cadence:           10 * time.Minute,
		ScheduleDelay:     10 * time.Minute,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     1 * time.Hour,
		RunTimeout:        5 * time.Minute,
		StaleTimeout:      15 * time.Minute,
		HeartbeatInterval: 2 * time.Minute,
		AllowStaleReentry: true,
		MaxAttempts:       2,
		RetryBackoff: []time.Duration{
			1 * time.Minute,
			5 * time.Minute,
		},
		Scopes:  StaticScopes(ScopeGlobal),
		Handler: makeIssueSyncPollHandler(engine),
	}
}

func makeIssueSyncPollHandler(engine *issuesync.Engine) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		engine.PollAllSources(ctx)
		if in.Heartbeat != nil {
			_ = in.Heartbeat(ctx)
		}
		return HandlerResult{RowsAffected: 0, Result: map[string]any{"polled": true}}, nil
	}
}
