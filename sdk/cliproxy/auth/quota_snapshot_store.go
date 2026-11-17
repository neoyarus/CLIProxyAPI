package auth

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	log "github.com/sirupsen/logrus"
)

// Passive quota snapshots (Auth.Quota.ObservedAt and Signals) are rebuilt from
// response headers and live only in memory, so a restart forgets every
// credential's usage until it serves another request. They are therefore saved
// to the KV store (local bbolt by default) and restored at startup.

const (
	quotaSnapshotKeyPrefix = "cpa:quota-snapshot:v1:"
	// quotaSnapshotSaveInterval bounds writes to one per credential per interval;
	// a restart loses at most this much of the newest snapshot.
	quotaSnapshotSaveInterval = time.Minute
	// quotaSnapshotTTL lets entries of removed credentials expire.
	quotaSnapshotTTL     = 30 * 24 * time.Hour
	quotaSnapshotTimeout = 5 * time.Second
)

// persistedQuotaSnapshot is one credential's saved passive quota snapshot.
type persistedQuotaSnapshot struct {
	AuthID     string            `json:"auth_id"`
	Provider   string            `json:"provider"`
	ObservedAt time.Time         `json:"observed_at"`
	Signals    map[string]string `json:"signals"`
}

func quotaSnapshotKey(authID string) string {
	return quotaSnapshotKeyPrefix + kvstore.HashKeyPart(authID)
}

// writeQuotaSnapshot saves a snapshot off the request path. Tests replace it.
var writeQuotaSnapshot = func(snapshot persistedQuotaSnapshot) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), quotaSnapshotTimeout)
		defer cancel()
		if _, errSet := kvstore.KVSetJSONRequired(ctx, quotaSnapshotKey(snapshot.AuthID), snapshot, quotaSnapshotTTL); errSet != nil {
			log.Debugf("save quota snapshot for %s: %v", snapshot.AuthID, errSet)
		}
	}()
}

// saveQuotaSnapshotLocked queues the credential's current passive snapshot for
// saving, at most once per quotaSnapshotSaveInterval per credential. m.mu is held.
func (m *Manager) saveQuotaSnapshotLocked(auth *Auth, now time.Time) {
	if m == nil || auth == nil || auth.ID == "" || auth.Quota.ObservedAt.IsZero() || len(auth.Quota.Signals) == 0 {
		return
	}
	if last, ok := m.quotaSnapshotSavedAt.Load(auth.ID); ok {
		if savedAt, okTime := last.(time.Time); okTime && now.Sub(savedAt) < quotaSnapshotSaveInterval {
			return
		}
	}
	m.quotaSnapshotSavedAt.Store(auth.ID, now)
	writeQuotaSnapshot(persistedQuotaSnapshot{
		AuthID:     auth.ID,
		Provider:   strings.ToLower(strings.TrimSpace(auth.Provider)),
		ObservedAt: auth.Quota.ObservedAt,
		Signals:    maps.Clone(auth.Quota.Signals),
	})
}

// RestoreQuotaSnapshots fills each credential's passive quota snapshot from the
// KV store when the saved one is newer than what the credential holds. It
// returns how many credentials were restored; without a KV store it does nothing.
func (m *Manager) RestoreQuotaSnapshots(ctx context.Context) (int, error) {
	if m == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, kvMode, errClient := kvstore.CurrentKVClient()
	if !kvMode || errClient != nil || client == nil {
		return 0, errClient
	}

	m.mu.RLock()
	ids := make([]string, 0, len(m.auths))
	for id, auth := range m.auths {
		if auth != nil && ProviderSupportsQuotaObservation(auth.Provider) {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	if len(ids) == 0 {
		return 0, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = quotaSnapshotKey(id)
	}
	values, found, errGet := client.KVMGet(ctx, keys...)
	if errGet != nil {
		return 0, errGet
	}

	restored := make([]*Auth, 0, len(ids))
	m.mu.Lock()
	for i, id := range ids {
		if i >= len(values) || i >= len(found) || !found[i] {
			continue
		}
		var snapshot persistedQuotaSnapshot
		if errUnmarshal := json.Unmarshal(values[i], &snapshot); errUnmarshal != nil {
			continue
		}
		auth := m.auths[id]
		if auth == nil || snapshot.AuthID != id || len(snapshot.Signals) == 0 || snapshot.ObservedAt.IsZero() ||
			!strings.EqualFold(snapshot.Provider, strings.TrimSpace(auth.Provider)) ||
			!snapshot.ObservedAt.After(auth.Quota.ObservedAt) {
			continue
		}
		auth.Quota.ObservedAt = snapshot.ObservedAt
		auth.Quota.Signals = maps.Clone(snapshot.Signals)
		restored = append(restored, auth.Clone())
	}
	m.mu.Unlock()

	if m.scheduler != nil {
		for _, snapshot := range restored {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	return len(restored), nil
}
