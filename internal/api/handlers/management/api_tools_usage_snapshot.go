package management

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Long-lived Claude setup tokens are inference-only, so Anthropic refuses their
// usage endpoint. api-call usage requests for them are answered from the
// credential's passive quota snapshot (the rate-limit headers of its latest
// response) in the usage endpoint's own format, without contacting upstream.

const (
	claudeUsageHost = "api.anthropic.com"
	claudeUsagePath = "/api/oauth/usage"
	// snapshotSourceHeader marks api-call responses served from the snapshot.
	snapshotSourceHeader = "X-Cpa-Usage-Source"
)

type claudeUsageWindowBody struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    *string `json:"resets_at"`
}

type claudeUsageBody struct {
	FiveHour *claudeUsageWindowBody `json:"five_hour"`
	SevenDay *claudeUsageWindowBody `json:"seven_day"`
}

func isClaudeUsageURL(u *url.URL) bool {
	if u == nil || !strings.EqualFold(u.Hostname(), claudeUsageHost) {
		return false
	}
	return strings.TrimSuffix(u.Path, "/") == claudeUsagePath
}

// claudeSetupTokenUsageResponse answers a Claude usage request for a setup-token
// credential from its quota snapshot. ok is false when the request is not one.
func claudeSetupTokenUsageResponse(auth *coreauth.Auth, method string, u *url.URL, now time.Time) (apiCallResponse, bool) {
	if auth == nil || method != http.MethodGet || !isClaudeUsageURL(u) || !isClaudeSetupTokenAuth(auth) {
		return apiCallResponse{}, false
	}
	header := map[string][]string{
		"Content-Type":       {"application/json"},
		snapshotSourceHeader: {"quota-snapshot"},
	}
	usage, ok := claudeUsageFromQuota(auth.Quota, now)
	if !ok {
		return apiCallResponse{
			StatusCode: http.StatusServiceUnavailable,
			Header:     header,
			Body:       `{"type":"error","error":{"type":"no_usage_snapshot","message":"No usage has been observed for this setup-token credential yet."}}`,
		}, true
	}
	body, errMarshal := json.Marshal(usage)
	if errMarshal != nil {
		return apiCallResponse{}, false
	}
	return apiCallResponse{StatusCode: http.StatusOK, Header: header, Body: string(body)}, true
}

// claudeUsageFromQuota converts Anthropic unified rate-limit signals (utilization
// as a 0..1 fraction, reset as unix seconds) into usage windows (utilization in
// percent, reset as RFC 3339). A reset already in the past means the window
// restarted after the snapshot: usage 0, reset moved forward by whole windows.
func claudeUsageFromQuota(quota coreauth.QuotaState, now time.Time) (claudeUsageBody, bool) {
	if len(quota.Signals) == 0 {
		return claudeUsageBody{}, false
	}
	lookup := make(map[string]string, len(quota.Signals))
	for key, value := range quota.Signals {
		lookup[strings.ToLower(key)] = value
	}
	window := func(name string, length time.Duration) *claudeUsageWindowBody {
		prefix := "anthropic-ratelimit-unified-" + name + "-"
		utilization, errParse := strconv.ParseFloat(strings.TrimSpace(lookup[prefix+"utilization"]), 64)
		if errParse != nil || math.IsNaN(utilization) || math.IsInf(utilization, 0) {
			return nil
		}
		out := &claudeUsageWindowBody{Utilization: math.Min(100, math.Max(0, utilization*100))}
		resetUnix, errReset := strconv.ParseFloat(strings.TrimSpace(lookup[prefix+"reset"]), 64)
		if errReset == nil && resetUnix > 0 {
			reset := time.Unix(int64(resetUnix), 0).UTC()
			if !reset.After(now) {
				out.Utilization = 0
				reset = reset.Add((now.Sub(reset)/length + 1) * length)
			}
			formatted := reset.Format(time.RFC3339)
			out.ResetsAt = &formatted
		}
		return out
	}
	usage := claudeUsageBody{
		FiveHour: window("5h", 5*time.Hour),
		SevenDay: window("7d", 7*24*time.Hour),
	}
	return usage, usage.FiveHour != nil || usage.SevenDay != nil
}
