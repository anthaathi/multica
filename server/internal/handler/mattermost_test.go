package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Mattermost has no dedicated publish wrapper (unlike Slack's
// publishSlackInstallationCreated): RegisterMattermostBYO and
// RevokeMattermostInstallation call h.publish inline with these exact
// arguments. These tests pin the event constant, envelope, and "id" payload so a
// wiring regression — wrong constant, dropped/renamed payload field — fails
// loudly. They mirror TestPublishSlackInstallationCreated. Bus.Publish is
// synchronous, so the subscriber fires inline.
func TestPublishMattermostInstallationCreated(t *testing.T) {
	bus := events.New()
	h := &Handler{Bus: bus}

	const (
		wsID   = "11111111-1111-1111-1111-111111111111"
		instID = "22222222-2222-2222-2222-222222222222"
	)

	var got events.Event
	fired := 0
	bus.Subscribe(protocol.EventMattermostInstallationCreated, func(e events.Event) {
		got = e
		fired++
	})

	// Mirrors the exact publish call in RegisterMattermostBYO.
	row := db.ChannelInstallation{
		ID:          parseUUID(instID),
		WorkspaceID: parseUUID(wsID),
	}
	h.publish(protocol.EventMattermostInstallationCreated, uuidToString(row.WorkspaceID), "user", "user-1", map[string]any{
		"id": uuidToString(row.ID),
	})

	if fired != 1 {
		t.Fatalf("expected mattermost_installation:created published once, got %d", fired)
	}
	if got.WorkspaceID != wsID || got.ActorType != "user" || got.ActorID != "user-1" {
		t.Errorf("event envelope = %+v", got)
	}
	payload, ok := got.Payload.(map[string]any)
	if !ok || payload["id"] != instID {
		t.Errorf("payload = %v, want installation id %s", got.Payload, instID)
	}
}

func TestPublishMattermostInstallationRevoked(t *testing.T) {
	bus := events.New()
	h := &Handler{Bus: bus}

	const (
		wsID   = "33333333-3333-3333-3333-333333333333"
		instID = "44444444-4444-4444-4444-444444444444"
	)

	var got events.Event
	fired := 0
	bus.Subscribe(protocol.EventMattermostInstallationRevoked, func(e events.Event) {
		got = e
		fired++
	})

	// Mirrors the exact publish call in RevokeMattermostInstallation.
	row := db.ChannelInstallation{
		ID:          parseUUID(instID),
		WorkspaceID: parseUUID(wsID),
	}
	h.publish(protocol.EventMattermostInstallationRevoked, uuidToString(row.WorkspaceID), "user", "user-2", map[string]any{
		"id": uuidToString(row.ID),
	})

	if fired != 1 {
		t.Fatalf("expected mattermost_installation:revoked published once, got %d", fired)
	}
	if got.WorkspaceID != wsID || got.ActorType != "user" || got.ActorID != "user-2" {
		t.Errorf("event envelope = %+v", got)
	}
	payload, ok := got.Payload.(map[string]any)
	if !ok || payload["id"] != instID {
		t.Errorf("payload = %v, want installation id %s", got.Payload, instID)
	}
}
