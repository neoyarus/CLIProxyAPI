package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var usageSnapshotNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func usageSnapshotQuota() coreauth.QuotaState {
	return coreauth.QuotaState{
		ObservedAt: usageSnapshotNow.Add(-time.Minute),
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.44",
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(usageSnapshotNow.Add(2*time.Hour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.87",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(usageSnapshotNow.Add(30*time.Hour).Unix(), 10),
		},
	}
}

func setupTokenAuth(id string, quota coreauth.QuotaState) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "claude",
		Metadata: map[string]any{"type": "claude", "is_setup_token": true, "access_token": "sk-ant-oat01-x", "refresh_token": ""},
		Quota:    quota,
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, errParse := url.Parse(raw)
	if errParse != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, errParse)
	}
	return u
}

func TestClaudeSetupTokenUsageResponse_Scope(t *testing.T) {
	usageURL := mustURL(t, "https://api.anthropic.com/api/oauth/usage?cedar_ember=1")
	setup := setupTokenAuth("setup", usageSnapshotQuota())
	oauth := &coreauth.Auth{ID: "oauth", Provider: "claude", Quota: usageSnapshotQuota(),
		Metadata: map[string]any{"access_token": "sk-ant-oat01-y", "refresh_token": "sk-ant-ort01-y"}}

	if _, ok := claudeSetupTokenUsageResponse(setup, http.MethodGet, usageURL, usageSnapshotNow); !ok {
		t.Fatal("setup-token usage GET was not answered from the snapshot")
	}
	for name, tc := range map[string]struct {
		auth   *coreauth.Auth
		method string
		url    string
	}{
		"oauth login goes upstream":  {oauth, http.MethodGet, usageURL.String()},
		"profile endpoint":           {setup, http.MethodGet, "https://api.anthropic.com/api/oauth/profile"},
		"other host":                 {setup, http.MethodGet, "https://example.com/api/oauth/usage"},
		"non-GET":                    {setup, http.MethodPost, usageURL.String()},
		"no credential for the call": {nil, http.MethodGet, usageURL.String()},
	} {
		if _, ok := claudeSetupTokenUsageResponse(tc.auth, tc.method, mustURL(t, tc.url), usageSnapshotNow); ok {
			t.Errorf("%s: answered from the snapshot, want passthrough", name)
		}
	}
}

func TestClaudeUsageFromQuota_RollsForwardPastResets(t *testing.T) {
	quota := usageSnapshotQuota()
	quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(usageSnapshotNow.Add(-time.Hour).Unix(), 10)
	usage, ok := claudeUsageFromQuota(quota, usageSnapshotNow)
	if !ok || usage.FiveHour == nil || usage.SevenDay == nil {
		t.Fatalf("claudeUsageFromQuota() = %+v, %v; want both windows", usage, ok)
	}
	if usage.FiveHour.Utilization != 0 || *usage.FiveHour.ResetsAt != usageSnapshotNow.Add(4*time.Hour).Format(time.RFC3339) {
		t.Fatalf("five_hour = %+v (%s), want 0%% resetting in 4h", usage.FiveHour, *usage.FiveHour.ResetsAt)
	}
	if usage.SevenDay.Utilization != 87 {
		t.Fatalf("seven_day utilization = %v, want 87", usage.SevenDay.Utilization)
	}
	if _, ok := claudeUsageFromQuota(coreauth.QuotaState{}, usageSnapshotNow); ok {
		t.Fatal("empty snapshot reported usage")
	}
}

func TestAPICall_ServesSetupTokenUsageFromSnapshot(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	withData := setupTokenAuth("claude-setup.json", coreauth.QuotaState{
		ObservedAt: time.Now().Add(-time.Minute),
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.44",
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.87",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(time.Now().Add(30*time.Hour).Unix(), 10),
		},
	})
	noData := setupTokenAuth("claude-setup-new.json", coreauth.QuotaState{})
	for _, auth := range []*coreauth.Auth{withData, noData} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", h.APICall)
	call := func(authIndex string) ojson.Result {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"auth_index": authIndex,
			"method":     "GET",
			"url":        "https://api.anthropic.com/api/oauth/usage",
			"header":     map[string]string{"Authorization": "Bearer $TOKEN$", "anthropic-beta": "oauth-2025-04-20"},
		})
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(payload)))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("api-call status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		return ojson.Parse(recorder.Body.String())
	}

	resp := call(withData.EnsureIndex())
	if got := resp.Get("status_code").Int(); got != http.StatusOK {
		t.Fatalf("status_code = %d, want 200; response = %s", got, resp.Raw)
	}
	if got := resp.Get("header.X-Cpa-Usage-Source.0").String(); got != "quota-snapshot" {
		t.Fatalf("source header = %q, want quota-snapshot (no upstream call)", got)
	}
	// The body is the usage endpoint's own JSON: utilization in percent, resets_at as RFC 3339.
	body := ojson.Parse(resp.Get("body").String())
	if body.Get("five_hour.utilization").Float() != 44 || body.Get("seven_day.utilization").Float() != 87 {
		t.Fatalf("usage body = %s, want five_hour 44 and seven_day 87", body.Raw)
	}
	if _, errTime := time.Parse(time.RFC3339, body.Get("seven_day.resets_at").String()); errTime != nil {
		t.Fatalf("seven_day.resets_at = %q, want RFC 3339", body.Get("seven_day.resets_at").String())
	}

	resp = call(noData.EnsureIndex())
	if got := resp.Get("status_code").Int(); got != http.StatusServiceUnavailable {
		t.Fatalf("status_code without a snapshot = %d, want 503; response = %s", got, resp.Raw)
	}
}
