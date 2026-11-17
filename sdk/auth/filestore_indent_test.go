package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFileTokenStoreSaveWritesIndentedJSON(t *testing.T) {
	baseDir := t.TempDir()
	fileName := "Claude N.json"
	path := filepath.Join(baseDir, fileName)
	if errWrite := os.WriteFile(path, []byte(`{"type":"claude","access_token":"old"}`), 0o600); errWrite != nil {
		t.Fatalf("write existing auth file: %v", errWrite)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auth := &cliproxyauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Metadata: map[string]any{"type": "claude", "access_token": "new"},
	}
	if _, errSave := store.Save(context.Background(), auth); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}

	persisted, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read saved auth file: %v", errRead)
	}
	want := "{\n  \"access_token\": \"new\",\n  \"disabled\": false,\n  \"type\": \"claude\"\n}"
	if string(persisted) != want {
		t.Fatalf("saved auth file =\n%s\nwant\n%s", persisted, want)
	}
}
