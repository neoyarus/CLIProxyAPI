package management

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAuthFilesList_ReportsClaudeSetupToken(t *testing.T) {
	cases := []struct {
		id         string
		provider   string
		metadata   map[string]any
		attributes map[string]string
		want       bool
	}{
		{id: "flagged", provider: "claude", metadata: map[string]any{"is_setup_token": true, "access_token": "sk-ant-oat01-a"}, want: true},
		{id: "flag-alias", provider: "claude", metadata: map[string]any{"setup_token": true}, want: true},
		{id: "auth-kind", provider: "claude", metadata: map[string]any{}, attributes: map[string]string{"auth_kind": "setup-token"}, want: true},
		{id: "no-refresh-token", provider: "claude", metadata: map[string]any{"access_token": "sk-ant-oat01-b", "refresh_token": ""}, want: true},
		{id: "oauth-login", provider: "claude", metadata: map[string]any{"access_token": "sk-ant-oat01-c", "refresh_token": "sk-ant-ort01-c"}},
		{id: "flag-false", provider: "claude", metadata: map[string]any{"is_setup_token": false, "access_token": "sk-ant-oat01-d", "refresh_token": "r"}},
		{id: "other-provider", provider: "codex", metadata: map[string]any{"is_setup_token": true, "access_token": "sk-ant-oat01-e"}},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, tc := range cases {
		attributes := map[string]string{"runtime_only": "true"}
		for key, value := range tc.attributes {
			attributes[key] = value
		}
		if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
			ID: tc.id, FileName: tc.id + ".json", Provider: tc.provider, Attributes: attributes, Metadata: tc.metadata,
		}); errRegister != nil {
			t.Fatalf("register auth %s: %v", tc.id, errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	router := oauthProviderTestRouter(h)
	for _, path := range []string{"/v8/management/credentials", "/v0/management/auth-files"} {
		files := listOAuthProviderTestFiles(t, router, path)
		for _, tc := range cases {
			file, ok := files[tc.id]
			if !ok {
				t.Fatalf("%s: missing credential %s", path, tc.id)
			}
			got := file.Get("setup_token")
			if got.Bool() != tc.want || (!tc.want && got.Exists()) {
				t.Errorf("%s %s: setup_token = %s, want %v", path, tc.id, got.Raw, tc.want)
			}
		}
	}
}
