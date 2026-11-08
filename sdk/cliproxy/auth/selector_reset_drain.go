package auth

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetDrainSelector prefers the credential whose usage window resets soonest.
//
// Quota that is about to reset is quota that will be lost if it is not spent, so
// draining the credential closest to its reset first keeps the credentials with
// distant resets in reserve. The ordering follows codex-lb's reset_drain strategy:
//
//  1. Credentials are bucketed by whole days until their long (weekly) window
//     resets, falling back to the short window when no long window is known.
//     The earliest bucket wins.
//  2. Within a bucket, the credential with more remaining short-window quota
//     wins, then more remaining long-window quota, then the earlier exact
//     reset, then the credential ID for a stable order.
//
// Reset times come from the passive quota snapshot (Auth.Quota.Signals) that
// Codex (x-codex-primary/secondary-*) and Claude
// (anthropic-ratelimit-unified-5h/7d-*) responses already populate. Because the
// snapshot is passive, a credential that has never been observed is picked once
// so its reset time becomes known; afterwards a credential without quota data
// ranks behind every credential with a known reset. Credentials whose snapshot
// shows a window at 100% usage rank last so a fresh cooldown is not needed to
// route around them.
type ResetDrainSelector struct {
	mu     sync.Mutex
	probed map[string]struct{}
	// now overrides the clock in tests.
	now func() time.Time
}

const (
	resetDrainBucketSize = 24 * time.Hour
	// maxResetDrainProbedEntries bounds the probe ledger so credentials that are
	// removed permanently cannot leak entries.
	maxResetDrainProbedEntries = 4096
)

const (
	resetDrainTierProbe = iota
	resetDrainTierKnown
	resetDrainTierUnknown
	resetDrainTierSaturated
)

// Pick selects the available credential whose usage resets soonest.
func (s *ResetDrainSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) == 1 {
		s.markProbed(available[0])
		return available[0], nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Auth
	var bestRank resetDrainRank
	for _, candidate := range available {
		if candidate == nil {
			continue
		}
		rank := s.rankLocked(candidate, now)
		if best == nil || rank.less(bestRank) {
			best = candidate
			bestRank = rank
		}
	}
	if best == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if bestRank.tier == resetDrainTierProbe {
		s.markProbedLocked(best.ID)
	}
	return best, nil
}

func (s *ResetDrainSelector) markProbed(auth *Auth) {
	if auth == nil {
		return
	}
	s.mu.Lock()
	s.markProbedLocked(auth.ID)
	s.mu.Unlock()
}

func (s *ResetDrainSelector) markProbedLocked(authID string) {
	if s.probed == nil || len(s.probed) >= maxResetDrainProbedEntries {
		s.probed = make(map[string]struct{})
	}
	s.probed[authID] = struct{}{}
}

type resetDrainRank struct {
	tier           int
	bucket         int64
	shortRemaining float64
	longRemaining  float64
	resetUnix      int64
	id             string
}

func (r resetDrainRank) less(other resetDrainRank) bool {
	if r.tier != other.tier {
		return r.tier < other.tier
	}
	if r.bucket != other.bucket {
		return r.bucket < other.bucket
	}
	if r.shortRemaining != other.shortRemaining {
		return r.shortRemaining > other.shortRemaining
	}
	if r.longRemaining != other.longRemaining {
		return r.longRemaining > other.longRemaining
	}
	if r.resetUnix != other.resetUnix {
		return r.resetUnix < other.resetUnix
	}
	return r.id < other.id
}

func (s *ResetDrainSelector) rankLocked(auth *Auth, now time.Time) resetDrainRank {
	rank := resetDrainRank{id: auth.ID, bucket: math.MaxInt64, resetUnix: math.MaxInt64}
	windows := usageWindowsFromQuota(auth.Quota, now)
	if len(windows) == 0 {
		rank.tier = resetDrainTierUnknown
		if _, probed := s.probed[auth.ID]; !probed && auth.Quota.ObservedAt.IsZero() {
			rank.tier = resetDrainTierProbe
		}
		return rank
	}

	short, long := windows[0], windows[len(windows)-1]
	rank.tier = resetDrainTierKnown
	for _, window := range windows {
		if window.usedPercent >= 100 {
			rank.tier = resetDrainTierSaturated
		}
	}
	rank.shortRemaining = short.remainingPercent()
	rank.longRemaining = long.remainingPercent()

	anchor := long
	if anchor.resetAt.IsZero() {
		anchor = short
	}
	if !anchor.resetAt.IsZero() {
		until := anchor.resetAt.Sub(now)
		if until < 0 {
			until = 0
		}
		rank.bucket = int64(until / resetDrainBucketSize)
		rank.resetUnix = anchor.resetAt.Unix()
	}
	return rank
}

// usageWindow is one rate-limit window decoded from a passive quota snapshot.
type usageWindow struct {
	// usedPercent is in [0, 100], or negative when unknown.
	usedPercent float64
	// resetAt is zero when unknown.
	resetAt time.Time
	// length is zero when unknown.
	length time.Duration
	// order breaks ties between windows of unknown length (primary before secondary).
	order int
}

func (w usageWindow) remainingPercent() float64 {
	if w.usedPercent < 0 {
		return 0
	}
	return math.Max(0, 100-w.usedPercent)
}

// usageWindowsFromQuota decodes the usage windows carried by a passive quota
// snapshot, sorted from the shortest window to the longest. Windows whose reset
// has already passed are rolled forward by their length (with usage cleared),
// because the snapshot predates the reset.
func usageWindowsFromQuota(quota QuotaState, now time.Time) []usageWindow {
	if len(quota.Signals) == 0 {
		return nil
	}
	signals := quota.Signals
	var windows []usageWindow

	for order, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		window := usageWindow{usedPercent: -1, order: order}
		present := false
		if used, ok := parseSignalFloat(signals, prefix+"Used-Percent"); ok {
			window.usedPercent = clampPercent(used)
			present = true
		}
		if minutes, ok := parseSignalFloat(signals, prefix+"Window-Minutes"); ok && minutes > 0 {
			window.length = time.Duration(minutes * float64(time.Minute))
		}
		if resetAt, ok := parseSignalUnix(signals, prefix+"Reset-At"); ok {
			window.resetAt = resetAt
			present = true
		} else if after, ok := parseSignalFloat(signals, prefix+"Reset-After-Seconds"); ok && after >= 0 && !quota.ObservedAt.IsZero() {
			window.resetAt = quota.ObservedAt.Add(time.Duration(after * float64(time.Second)))
			present = true
		}
		if present {
			windows = append(windows, window)
		}
	}

	for order, spec := range []struct {
		name   string
		length time.Duration
	}{{"5h", 5 * time.Hour}, {"7d", 7 * 24 * time.Hour}} {
		prefix := "Anthropic-Ratelimit-Unified-" + spec.name + "-"
		window := usageWindow{usedPercent: -1, length: spec.length, order: order}
		present := false
		if utilization, ok := parseSignalFloat(signals, prefix+"Utilization"); ok {
			window.usedPercent = clampPercent(utilization * 100)
			present = true
		}
		if resetAt, ok := parseSignalUnix(signals, prefix+"Reset"); ok {
			window.resetAt = resetAt
			present = true
		}
		if present {
			windows = append(windows, window)
		}
	}

	for i := range windows {
		windows[i] = windows[i].rolledForward(now)
	}
	sort.SliceStable(windows, func(i, j int) bool {
		left, right := windows[i], windows[j]
		if left.length != right.length && left.length > 0 && right.length > 0 {
			return left.length < right.length
		}
		return left.order < right.order
	})
	return windows
}

func (w usageWindow) rolledForward(now time.Time) usageWindow {
	if w.resetAt.IsZero() || w.resetAt.After(now) {
		return w
	}
	w.usedPercent = 0
	if w.length <= 0 {
		w.resetAt = time.Time{}
		return w
	}
	elapsed := now.Sub(w.resetAt)
	periods := elapsed/w.length + 1
	w.resetAt = w.resetAt.Add(periods * w.length)
	return w
}

func parseSignalFloat(signals map[string]string, key string) (float64, bool) {
	raw, ok := signals[key]
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func parseSignalUnix(signals map[string]string, key string) (time.Time, bool) {
	value, ok := parseSignalFloat(signals, key)
	if !ok || value <= 0 {
		return time.Time{}, false
	}
	// Accept millisecond timestamps defensively; seconds are expected.
	if value > 1e12 {
		value /= 1000
	}
	return time.Unix(int64(value), 0), true
}

func clampPercent(value float64) float64 {
	return math.Min(100, math.Max(0, value))
}
