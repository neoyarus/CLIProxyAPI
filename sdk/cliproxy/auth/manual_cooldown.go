package auth

import (
	"strings"
	"time"
)

// ManualCooldownMetadataKey holds an operator-set pause on a credential: the RFC 3339
// time until which it is treated as cooling down. It lives in the credential's own
// metadata (its auth file), so it survives restarts and file reloads, and it ends by
// itself once the time has passed.
const ManualCooldownMetadataKey = "cooldown_until"

// MaxManualCooldown bounds how far ahead a manual pause may end.
const MaxManualCooldown = 31 * 24 * time.Hour

// ManualCooldownUntil returns the end of the credential's manual pause, or zero when it
// has none or the value is unreadable.
func ManualCooldownUntil(auth *Auth) time.Time {
	if auth == nil || auth.Metadata == nil {
		return time.Time{}
	}
	raw, _ := auth.Metadata[ManualCooldownMetadataKey].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	until, errParse := time.Parse(time.RFC3339, raw)
	if errParse != nil {
		return time.Time{}
	}
	return until
}

// SetManualCooldown pauses the credential until the given time, or clears the pause
// when until is zero.
func SetManualCooldown(auth *Auth, until time.Time) {
	if auth == nil {
		return
	}
	if until.IsZero() {
		if auth.Metadata != nil {
			delete(auth.Metadata, ManualCooldownMetadataKey)
		}
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[ManualCooldownMetadataKey] = until.UTC().Format(time.RFC3339)
}
