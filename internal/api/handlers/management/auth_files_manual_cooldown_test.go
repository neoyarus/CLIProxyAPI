package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSetCredentialCooldown_PauseListAndResume(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID: "claude-n.json", FileName: "claude-n.json", Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"type": "claude", "note": "N"},
	}); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	router := gin.New()
	router.POST("/cooldown", h.SetCredentialCooldown)
	router.GET("/list", h.ListAuthFiles)

	post := func(body string) (int, ojson.Result) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/cooldown", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		return recorder.Code, ojson.Parse(recorder.Body.String())
	}
	listed := func() ojson.Result {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/list", nil))
		for _, file := range ojson.Parse(recorder.Body.String()).Get("files").Array() {
			if file.Get("name").String() == "claude-n.json" {
				return file
			}
		}
		t.Fatalf("credential missing from list: %s", recorder.Body.String())
		return ojson.Result{}
	}

	before := time.Now()
	code, resp := post(`{"name":"claude-n.json","duration_seconds":7200}`)
	if code != http.StatusOK {
		t.Fatalf("pause status = %d, body = %s", code, resp.Raw)
	}
	until, errParse := time.Parse(time.RFC3339, resp.Get("cooldown_until").String())
	if errParse != nil || until.Before(before.Add(2*time.Hour-time.Second)) || until.After(time.Now().Add(2*time.Hour+time.Second)) {
		t.Fatalf("cooldown_until = %q, want about 2h from now", resp.Get("cooldown_until").String())
	}
	if got := listed().Get("cooldown_until").String(); got != resp.Get("cooldown_until").String() {
		t.Fatalf("listed cooldown_until = %q, want %q", got, resp.Get("cooldown_until").String())
	}
	stored, _ := manager.GetByID("claude-n.json")
	if !coreauth.ManualCooldownUntil(stored).Equal(until) {
		t.Fatalf("stored cooldown = %v, want %v", coreauth.ManualCooldownUntil(stored), until)
	}

	code, resp = post(`{"name":"claude-n.json"}`)
	if code != http.StatusOK || resp.Get("cooldown_until").String() != "" {
		t.Fatalf("resume = %d %s, want 200 with empty cooldown_until", code, resp.Raw)
	}
	if listed().Get("cooldown_until").Exists() {
		t.Fatal("resumed credential still lists cooldown_until")
	}

	for body, want := range map[string]int{
		`{"name":"claude-n.json","until":"2000-01-01T00:00:00Z"}`:                       http.StatusBadRequest,
		`{"name":"claude-n.json","until":"tomorrow"}`:                                   http.StatusBadRequest,
		`{"name":"claude-n.json","duration_seconds":-5}`:                                http.StatusBadRequest,
		`{"name":"claude-n.json","duration_seconds":99999999}`:                          http.StatusBadRequest,
		`{"name":"claude-n.json","until":"2099-01-01T00:00:00Z","duration_seconds":60}`: http.StatusBadRequest,
		`{"duration_seconds":60}`:                                                       http.StatusBadRequest,
		`{"name":"missing.json","duration_seconds":60}`:                                 http.StatusNotFound,
	} {
		if code, resp := post(body); code != want {
			t.Errorf("POST %s = %d (%s), want %d", body, code, resp.Raw, want)
		}
	}
}
