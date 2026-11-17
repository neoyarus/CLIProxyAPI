package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManualCooldown_SetReadClear(t *testing.T) {
	auth := &Auth{ID: "a"}
	if !ManualCooldownUntil(auth).IsZero() {
		t.Fatal("new credential has a manual cooldown")
	}
	until := time.Date(2026, 11, 17, 18, 30, 0, 0, time.FixedZone("MSK", 3*3600))
	SetManualCooldown(auth, until)
	if got := auth.Metadata[ManualCooldownMetadataKey]; got != "2026-11-17T15:30:00Z" {
		t.Fatalf("stored value = %v, want UTC RFC 3339", got)
	}
	if !ManualCooldownUntil(auth).Equal(until) {
		t.Fatalf("ManualCooldownUntil() = %v, want %v", ManualCooldownUntil(auth), until)
	}
	SetManualCooldown(auth, time.Time{})
	if _, ok := auth.Metadata[ManualCooldownMetadataKey]; ok {
		t.Fatal("clearing left the metadata key")
	}
	auth.Metadata[ManualCooldownMetadataKey] = "not a time"
	if !ManualCooldownUntil(auth).IsZero() {
		t.Fatal("unreadable value treated as a cooldown")
	}
}

func TestManualCooldown_BlocksUntilItEnds(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	auth := &Auth{ID: "a", Provider: "claude", Status: StatusActive}
	SetManualCooldown(auth, now.Add(2*time.Hour))

	blocked, reason, next := isAuthBlockedForModel(auth, "claude-opus-5-5", now)
	if !blocked || reason != blockReasonCooldown || !next.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("isAuthBlockedForModel() = %v, %v, %v; want blocked by cooldown until +2h", blocked, reason, next)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "claude-opus-5-5", now.Add(2*time.Hour+time.Second)); blocked {
		t.Fatal("credential still blocked after the cooldown ended")
	}
}

func TestManualCooldown_ResetPaceSkipsPausedCredential(t *testing.T) {
	selector := newResetPaceTestSelector()
	// "soon" would win: on pace with the nearest reset. Pausing it hands traffic to "later".
	soon := codexAuth("soon", 0, time.Hour, 60, 2*24*time.Hour)
	later := codexAuth("later", 0, time.Hour, 10, 5*24*time.Hour)
	for _, auth := range []*Auth{soon, later} {
		auth.Provider = "codex"
		auth.Status = StatusActive
	}
	if got := pickResetDrain(t, selector, soon, later); got != "soon" {
		t.Fatalf("Pick() before pause = %q, want soon", got)
	}
	SetManualCooldown(soon, resetDrainTestNow.Add(time.Hour))
	if got := pickResetDrain(t, selector, soon, later); got != "later" {
		t.Fatalf("Pick() with soon paused = %q, want later", got)
	}

	SetManualCooldown(later, resetDrainTestNow.Add(30*time.Minute))
	if _, errPick := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{soon, later}); errPick == nil {
		t.Fatal("Pick() with every credential paused succeeded, want a cooldown error")
	}
}
