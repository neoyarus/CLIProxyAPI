package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

func useLocalKVStore(t *testing.T) {
	t.Helper()
	if errConfigure := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "kv.db")); errConfigure != nil {
		t.Fatalf("ConfigureLocal() error = %v", errConfigure)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })
}

func captureQuotaSnapshotWrites(t *testing.T) *[]persistedQuotaSnapshot {
	t.Helper()
	var writes []persistedQuotaSnapshot
	original := writeQuotaSnapshot
	writeQuotaSnapshot = func(snapshot persistedQuotaSnapshot) { writes = append(writes, snapshot) }
	t.Cleanup(func() { writeQuotaSnapshot = original })
	return &writes
}

func TestSaveQuotaSnapshotLocked_ThrottlesPerCredential(t *testing.T) {
	writes := captureQuotaSnapshotWrites(t)
	m := NewManager(nil, nil, nil)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	auth := &Auth{ID: "claude-a.json", Provider: "Claude", Quota: QuotaState{
		ObservedAt: now,
		Signals:    map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.4"},
	}}

	m.saveQuotaSnapshotLocked(auth, now)
	auth.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0.5"
	m.saveQuotaSnapshotLocked(auth, now.Add(30*time.Second))
	m.saveQuotaSnapshotLocked(&Auth{ID: "claude-b.json", Provider: "claude", Quota: auth.Quota}, now.Add(30*time.Second))
	m.saveQuotaSnapshotLocked(auth, now.Add(61*time.Second))
	m.saveQuotaSnapshotLocked(&Auth{ID: "empty.json", Provider: "claude"}, now)

	if len(*writes) != 3 {
		t.Fatalf("writes = %d, want 3 (first, other credential, after the interval)", len(*writes))
	}
	first := (*writes)[0]
	if first.AuthID != "claude-a.json" || first.Provider != "claude" ||
		first.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] != "0.4" {
		t.Fatalf("first write = %+v, want an independent copy of the 0.4 snapshot", first)
	}
	if (*writes)[2].Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] != "0.5" {
		t.Fatalf("third write = %+v, want the newer 0.5 snapshot", (*writes)[2])
	}
}

func TestRestoreQuotaSnapshots_FillsCredentialsFromKVStore(t *testing.T) {
	useLocalKVStore(t)
	ctx := context.Background()
	saved := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	put := func(snapshot persistedQuotaSnapshot) {
		t.Helper()
		if _, errSet := kvstore.KVSetJSONRequired(ctx, quotaSnapshotKey(snapshot.AuthID), snapshot, quotaSnapshotTTL); errSet != nil {
			t.Fatalf("KVSetJSONRequired() error = %v", errSet)
		}
	}
	signals := map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.8"}
	put(persistedQuotaSnapshot{AuthID: "fresh", Provider: "claude", ObservedAt: saved, Signals: signals})
	put(persistedQuotaSnapshot{AuthID: "newer-in-memory", Provider: "claude", ObservedAt: saved, Signals: signals})
	put(persistedQuotaSnapshot{AuthID: "other-provider", Provider: "codex", ObservedAt: saved, Signals: signals})

	m := NewManager(nil, nil, nil)
	register := func(auth *Auth) {
		t.Helper()
		if _, errRegister := m.Register(ctx, auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	register(&Auth{ID: "fresh", Provider: "claude", Status: StatusActive})
	register(&Auth{ID: "newer-in-memory", Provider: "claude", Status: StatusActive, Quota: QuotaState{
		ObservedAt: saved.Add(time.Minute),
		Signals:    map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.9"},
	}})
	register(&Auth{ID: "other-provider", Provider: "claude", Status: StatusActive})
	register(&Auth{ID: "never-saved", Provider: "claude", Status: StatusActive})

	restored, errRestore := m.RestoreQuotaSnapshots(ctx)
	if errRestore != nil {
		t.Fatalf("RestoreQuotaSnapshots() error = %v", errRestore)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}
	quota := func(id string) QuotaState {
		auth, ok := m.GetByID(id)
		if !ok {
			t.Fatalf("GetByID(%s) not found", id)
		}
		return auth.Quota
	}
	if got := quota("fresh"); !got.ObservedAt.Equal(saved) || got.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] != "0.8" {
		t.Fatalf("fresh quota = %+v, want the saved snapshot", got)
	}
	if got := quota("newer-in-memory"); got.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] != "0.9" {
		t.Fatalf("newer-in-memory quota = %+v, want its own newer snapshot kept", got)
	}
	if got := quota("other-provider"); !got.ObservedAt.IsZero() {
		t.Fatalf("other-provider quota = %+v, want no snapshot from a different provider", got)
	}
	if got := quota("never-saved"); !got.ObservedAt.IsZero() {
		t.Fatalf("never-saved quota = %+v, want none", got)
	}
}

func TestRestoreQuotaSnapshots_RoundTripAcrossManagers(t *testing.T) {
	useLocalKVStore(t)
	ctx := context.Background()
	writes := captureQuotaSnapshotWrites(t)
	observed := time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)

	before := NewManager(nil, nil, nil)
	auth := &Auth{ID: "claude-j.json", Provider: "claude", Quota: QuotaState{
		ObservedAt: observed,
		Signals:    map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.44"},
	}}
	before.saveQuotaSnapshotLocked(auth, observed)
	for _, snapshot := range *writes {
		if _, errSet := kvstore.KVSetJSONRequired(ctx, quotaSnapshotKey(snapshot.AuthID), snapshot, quotaSnapshotTTL); errSet != nil {
			t.Fatalf("KVSetJSONRequired() error = %v", errSet)
		}
	}

	// A fresh manager stands in for the restarted process.
	after := NewManager(nil, nil, nil)
	if _, errRegister := after.Register(ctx, &Auth{ID: "claude-j.json", Provider: "claude", Status: StatusActive}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if restored, errRestore := after.RestoreQuotaSnapshots(ctx); errRestore != nil || restored != 1 {
		t.Fatalf("RestoreQuotaSnapshots() = %d, %v; want 1, nil", restored, errRestore)
	}
	got, _ := after.GetByID("claude-j.json")
	if !got.Quota.ObservedAt.Equal(observed) || got.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] != "0.44" {
		t.Fatalf("restored quota = %+v, want the saved snapshot", got.Quota)
	}
}

func TestRestoreQuotaSnapshots_NoKVStoreIsANoop(t *testing.T) {
	if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
		t.Fatalf("ShutdownLocal() error = %v", errShutdown)
	}
	m := NewManager(nil, nil, nil)
	if restored, errRestore := m.RestoreQuotaSnapshots(context.Background()); restored != 0 || errRestore != nil {
		t.Fatalf("RestoreQuotaSnapshots() = %d, %v; want 0, nil without a KV store", restored, errRestore)
	}
}
