package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var resetDrainTestNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func newResetDrainTestSelector() *ResetDrainSelector {
	return &ResetDrainSelector{now: func() time.Time { return resetDrainTestNow }}
}

func unixString(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

// codexAuth builds a credential whose snapshot carries a 5h primary and a weekly
// secondary window, as Codex reports them.
func codexAuth(id string, primaryUsed float64, primaryReset time.Duration, secondaryUsed float64, secondaryReset time.Duration) *Auth {
	return &Auth{
		ID: id,
		Quota: QuotaState{
			ObservedAt: resetDrainTestNow.Add(-time.Minute),
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":     strconv.FormatFloat(primaryUsed, 'f', -1, 64),
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Primary-Reset-At":         unixString(resetDrainTestNow.Add(primaryReset)),
				"X-Codex-Secondary-Used-Percent":   strconv.FormatFloat(secondaryUsed, 'f', -1, 64),
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       unixString(resetDrainTestNow.Add(secondaryReset)),
			},
		},
	}
}

func pickResetDrain(t *testing.T, selector *ResetDrainSelector, auths ...*Auth) string {
	t.Helper()
	got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatal("Pick() auth = nil")
	}
	return got.ID
}

func TestResetDrainSelector_PrefersSoonestWeeklyReset(t *testing.T) {
	t.Parallel()

	// "soon" has spent more of its weekly quota, but that quota resets in one
	// day, so it must be drained before credentials that carry over longer.
	soon := codexAuth("soon", 10, 2*time.Hour, 80, 26*time.Hour)
	later := codexAuth("later", 0, 4*time.Hour, 5, 5*24*time.Hour)
	latest := codexAuth("latest", 0, 4*time.Hour, 0, 6*24*time.Hour)

	if got := pickResetDrain(t, newResetDrainTestSelector(), latest, later, soon); got != "soon" {
		t.Fatalf("Pick() = %q, want soon", got)
	}
}

func TestResetDrainSelector_SameDayPrefersMoreRemainingQuota(t *testing.T) {
	t.Parallel()

	// Both weekly windows reset within the same day bucket. The tie is broken
	// by remaining short-window quota first, as in codex-lb's reset_drain.
	nearlyCapped := codexAuth("nearly-capped", 95, time.Hour, 20, 30*time.Hour)
	roomy := codexAuth("roomy", 10, 3*time.Hour, 60, 40*time.Hour)

	if got := pickResetDrain(t, newResetDrainTestSelector(), nearlyCapped, roomy); got != "roomy" {
		t.Fatalf("Pick() = %q, want roomy", got)
	}
}

func TestResetDrainSelector_StaleResetRollsForward(t *testing.T) {
	t.Parallel()

	// The snapshot says the weekly window reset an hour ago, so its next reset
	// is about seven days away and it must rank behind a reset due in three days.
	stale := codexAuth("stale", 0, -time.Hour, 99, -time.Hour)
	current := codexAuth("current", 0, 2*time.Hour, 50, 3*24*time.Hour)

	if got := pickResetDrain(t, newResetDrainTestSelector(), stale, current); got != "current" {
		t.Fatalf("Pick() = %q, want current", got)
	}

	windows := usageWindowsFromQuota(stale.Quota, resetDrainTestNow)
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}
	weekly := windows[1]
	if weekly.usedPercent != 0 {
		t.Fatalf("rolled weekly usedPercent = %v, want 0", weekly.usedPercent)
	}
	if want := resetDrainTestNow.Add(-time.Hour + 7*24*time.Hour); !weekly.resetAt.Equal(want) {
		t.Fatalf("rolled weekly resetAt = %v, want %v", weekly.resetAt, want)
	}
}

func TestResetDrainSelector_ProbesUnobservedCredentialOnce(t *testing.T) {
	t.Parallel()

	selector := newResetDrainTestSelector()
	known := codexAuth("known", 0, time.Hour, 0, 24*time.Hour)
	unknown := &Auth{ID: "unknown"}

	if got := pickResetDrain(t, selector, known, unknown); got != "unknown" {
		t.Fatalf("first Pick() = %q, want unknown (probe)", got)
	}
	if got := pickResetDrain(t, selector, known, unknown); got != "known" {
		t.Fatalf("second Pick() = %q, want known after probe", got)
	}
	// An unknown credential still serves when it is the only one left.
	if got := pickResetDrain(t, selector, unknown); got != "unknown" {
		t.Fatalf("Pick() with only unknown = %q, want unknown", got)
	}
}

func TestResetDrainSelector_SaturatedRanksLast(t *testing.T) {
	t.Parallel()

	selector := newResetDrainTestSelector()
	selector.markProbedLocked("no-data")
	saturated := codexAuth("saturated", 100, time.Hour, 40, 12*time.Hour)
	noData := &Auth{ID: "no-data"}
	healthy := codexAuth("healthy", 0, time.Hour, 0, 6*24*time.Hour)

	if got := pickResetDrain(t, selector, saturated, noData, healthy); got != "healthy" {
		t.Fatalf("Pick() = %q, want healthy", got)
	}
	if got := pickResetDrain(t, selector, saturated, noData); got != "no-data" {
		t.Fatalf("Pick() = %q, want no-data ahead of saturated", got)
	}
}

func TestResetDrainSelector_ClaudeUnifiedHeaders(t *testing.T) {
	t.Parallel()

	claudeAuth := func(id string, weeklyUtilization string, weeklyReset time.Duration) *Auth {
		return &Auth{
			ID: id,
			Quota: QuotaState{
				ObservedAt: resetDrainTestNow,
				Signals: map[string]string{
					"Anthropic-Ratelimit-Unified-5h-Utilization": "0.10",
					"Anthropic-Ratelimit-Unified-5h-Reset":       unixString(resetDrainTestNow.Add(3 * time.Hour)),
					"Anthropic-Ratelimit-Unified-7d-Utilization": weeklyUtilization,
					"Anthropic-Ratelimit-Unified-7d-Reset":       unixString(resetDrainTestNow.Add(weeklyReset)),
				},
			},
		}
	}
	far := claudeAuth("far", "0.05", 6*24*time.Hour)
	near := claudeAuth("near", "0.69", 2*24*time.Hour)

	if got := pickResetDrain(t, newResetDrainTestSelector(), far, near); got != "near" {
		t.Fatalf("Pick() = %q, want near", got)
	}
	windows := usageWindowsFromQuota(near.Quota, resetDrainTestNow)
	if len(windows) != 2 || windows[1].length != 7*24*time.Hour || windows[1].usedPercent != 69 {
		t.Fatalf("claude windows = %+v, want weekly window at 69%%", windows)
	}
}

func TestResetDrainSelector_ResetAfterSecondsUsesObservedAt(t *testing.T) {
	t.Parallel()

	observed := resetDrainTestNow.Add(-2 * time.Hour)
	auth := &Auth{
		ID: "relative",
		Quota: QuotaState{
			ObservedAt: observed,
			Signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":        "30",
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "86400",
			},
		},
	}
	windows := usageWindowsFromQuota(auth.Quota, resetDrainTestNow)
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(windows))
	}
	if want := observed.Add(24 * time.Hour); !windows[0].resetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", windows[0].resetAt, want)
	}
}

func TestResetDrainSelector_ManagerPicksSoonestReset(t *testing.T) {
	t.Parallel()

	const provider = "resetdrain-test"
	const model = "resetdrain-test-model"
	now := time.Now()
	makeAuth := func(id string, weeklyReset time.Duration) *Auth {
		return &Auth{
			ID:       id,
			Provider: provider,
			Status:   StatusActive,
			Metadata: map[string]any{"access_token": "token-" + id},
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"X-Codex-Secondary-Used-Percent":   "10",
					"X-Codex-Secondary-Window-Minutes": "10080",
					"X-Codex-Secondary-Reset-At":       unixString(now.Add(weeklyReset)),
				},
			},
		}
	}
	auths := []*Auth{
		makeAuth("resetdrain-mgr-a", 5*24*time.Hour),
		makeAuth("resetdrain-mgr-b", 30*time.Hour),
		makeAuth("resetdrain-mgr-c", 3*24*time.Hour),
	}

	manager := NewManager(nil, &ResetDrainSelector{}, nil)
	if manager.useSchedulerFastPath() {
		t.Fatal("useSchedulerFastPath() = true, want false for reset-drain")
	}
	manager.RegisterExecutor(&refreshMockExecutor{id: provider})
	for _, auth := range auths {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}

	for i := 0; i < 3; i++ {
		picked, _, err := manager.pickNext(context.Background(), provider, model, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNext() error = %v", err)
		}
		if picked.ID != "resetdrain-mgr-b" {
			t.Fatalf("pickNext() = %q, want resetdrain-mgr-b", picked.ID)
		}
	}

	tried := map[string]struct{}{"resetdrain-mgr-b": {}}
	picked, _, err := manager.pickNext(context.Background(), provider, model, cliproxyexecutor.Options{}, tried)
	if err != nil {
		t.Fatalf("pickNext() with tried error = %v", err)
	}
	if picked.ID != "resetdrain-mgr-c" {
		t.Fatalf("pickNext() after excluding b = %q, want resetdrain-mgr-c", picked.ID)
	}
}

func newResetPaceTestSelector() *ResetDrainSelector {
	return &ResetDrainSelector{Paced: true, now: func() time.Time { return resetDrainTestNow }}
}

func TestResetPaceSelector_SkipsSoonestResetWhenAheadOfPace(t *testing.T) {
	t.Parallel()

	// "soon" resets in 2 days, so 5/7 (71%) of its week has passed, but it has
	// already used 90%. "later" is 2/7 (29%) through its week at 20% used.
	soon := codexAuth("soon", 0, time.Hour, 90, 2*24*time.Hour)
	later := codexAuth("later", 0, time.Hour, 20, 5*24*time.Hour)

	if got := pickResetDrain(t, newResetDrainTestSelector(), later, soon); got != "soon" {
		t.Fatalf("reset-drain Pick() = %q, want soon", got)
	}
	if got := pickResetDrain(t, newResetPaceTestSelector(), soon, later); got != "later" {
		t.Fatalf("reset-pace Pick() = %q, want later (soon is ahead of pace)", got)
	}
}

func TestResetPaceSelector_OnPacePrefersSoonestReset(t *testing.T) {
	t.Parallel()

	// Both are on pace, so the nearest reset wins as in reset-drain.
	soon := codexAuth("soon", 0, time.Hour, 60, 2*24*time.Hour)
	later := codexAuth("later", 0, time.Hour, 10, 5*24*time.Hour)

	if got := pickResetDrain(t, newResetPaceTestSelector(), later, soon); got != "soon" {
		t.Fatalf("Pick() = %q, want soon", got)
	}
}

func TestResetPaceSelector_AllAheadPrefersLeastAhead(t *testing.T) {
	t.Parallel()

	// "soon" is 71% through its week at 80% used (9 points ahead); "later" is
	// 29% through at 33% used (4 points ahead), so "later" is picked.
	soon := codexAuth("soon", 0, time.Hour, 80, 2*24*time.Hour)
	later := codexAuth("later", 0, time.Hour, 33, 5*24*time.Hour)

	if got := pickResetDrain(t, newResetPaceTestSelector(), soon, later); got != "later" {
		t.Fatalf("Pick() = %q, want later (least ahead of pace)", got)
	}
}

func TestResetPaceSelector_FreshWindowIsOnPace(t *testing.T) {
	t.Parallel()

	// A window that just reset (stale snapshot rolled forward) has 0% used and
	// counts as on pace, but its reset is a week away so it ranks behind other
	// on-pace credentials.
	fresh := codexAuth("fresh", 0, -time.Hour, 99, -time.Hour)
	onPace := codexAuth("on-pace", 0, time.Hour, 50, 3*24*time.Hour)
	ahead := codexAuth("ahead", 0, time.Hour, 95, 24*time.Hour)

	if got := pickResetDrain(t, newResetPaceTestSelector(), fresh, ahead, onPace); got != "on-pace" {
		t.Fatalf("Pick() = %q, want on-pace", got)
	}
	if got := pickResetDrain(t, newResetPaceTestSelector(), fresh, ahead); got != "fresh" {
		t.Fatalf("Pick() = %q, want fresh ahead of the over-pace credential", got)
	}
}

func TestResetPaceSelector_UnknownWindowLengthCountsAsOnPace(t *testing.T) {
	t.Parallel()

	// Without a window length the pace cannot be judged, so the credential keeps
	// its reset-drain position instead of being pushed behind.
	noLength := &Auth{
		ID: "no-length",
		Quota: QuotaState{
			ObservedAt: resetDrainTestNow,
			Signals: map[string]string{
				"X-Codex-Secondary-Used-Percent": "95",
				"X-Codex-Secondary-Reset-At":     unixString(resetDrainTestNow.Add(24 * time.Hour)),
			},
		},
	}
	ahead := codexAuth("ahead", 0, time.Hour, 90, 3*24*time.Hour)

	if got := pickResetDrain(t, newResetPaceTestSelector(), ahead, noLength); got != "no-length" {
		t.Fatalf("Pick() = %q, want no-length", got)
	}
}

func TestResetPaceSelector_ClaudeWeeklyPace(t *testing.T) {
	t.Parallel()

	claudeAuth := func(id string, weeklyUtilization string, weeklyReset time.Duration) *Auth {
		return &Auth{
			ID: id,
			Quota: QuotaState{
				ObservedAt: resetDrainTestNow,
				Signals: map[string]string{
					"Anthropic-Ratelimit-Unified-5h-Utilization": "0.10",
					"Anthropic-Ratelimit-Unified-5h-Reset":       unixString(resetDrainTestNow.Add(3 * time.Hour)),
					"Anthropic-Ratelimit-Unified-7d-Utilization": weeklyUtilization,
					"Anthropic-Ratelimit-Unified-7d-Reset":       unixString(resetDrainTestNow.Add(weeklyReset)),
				},
			},
		}
	}
	// "near" is 4/7 (57%) through its week at 69% used, so it is ahead of pace.
	near := claudeAuth("near", "0.69", 3*24*time.Hour)
	far := claudeAuth("far", "0.05", 6*24*time.Hour)

	if got := pickResetDrain(t, newResetPaceTestSelector(), near, far); got != "far" {
		t.Fatalf("Pick() = %q, want far", got)
	}
}

func TestUsageWindowPaceOvershoot(t *testing.T) {
	t.Parallel()

	week := 7 * 24 * time.Hour
	half := usageWindow{usedPercent: 50, length: week, resetAt: resetDrainTestNow.Add(week / 2)}
	if got, ok := half.paceOvershoot(resetDrainTestNow); !ok || got != 0 {
		t.Fatalf("half-way at 50%% = %v, %v; want 0, true", got, ok)
	}
	half.usedPercent = 60
	if got, _ := half.paceOvershoot(resetDrainTestNow); got != 10 {
		t.Fatalf("half-way at 60%% = %v, want 10", got)
	}
	// A reset further away than the window length (clock skew) clamps to 0% elapsed.
	skewed := usageWindow{usedPercent: 5, length: week, resetAt: resetDrainTestNow.Add(week + time.Hour)}
	if got, _ := skewed.paceOvershoot(resetDrainTestNow); got != 5 {
		t.Fatalf("skewed overshoot = %v, want 5", got)
	}
	if _, ok := (usageWindow{usedPercent: -1, length: week, resetAt: resetDrainTestNow}).paceOvershoot(resetDrainTestNow); ok {
		t.Fatal("unknown usage reported a pace")
	}
}
