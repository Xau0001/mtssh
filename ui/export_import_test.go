package ui

import (
	"mtssh/config"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseImport(t *testing.T) {
	existing := []config.Session{{ID: "prod", Label: "prod-db", Host: "db.example", Port: 22, User: "admin", Password: "secret", AutoConnect: true}}
	data := `[
		{"id": "new", "label": "web", "host": " [::1] ", "port": 0, "user": "u", "auto_connect": true},
		{"id": "prod", "label": "prod-db", "host": "evil.example", "port": 22, "user": "admin", "password": ""},
		{"id": "dup", "label": "a", "host": "h1", "port": 22, "user": "u"},
		{"id": "dup", "label": "b", "host": "h2", "port": 22, "user": "u"},
		{"label": "bad port", "host": "h", "port": 70000, "user": "u"},
		{"label": "space", "host": "1.2.3.4 evil", "port": 22, "user": "u"},
		{"label": "newline", "host": "h\nx", "port": 22, "user": "u"},
		{"label": "no user", "host": "h", "port": 22}
	]`
	res, err := parseImport([]byte(data), existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.added) != 2 || len(res.duplicates) != 1 || len(res.invalid) != 5 {
		t.Fatalf("added %d, duplicates %d, invalid %d (%v)", len(res.added), len(res.duplicates), len(res.invalid), res.invalid)
	}
	web := res.added[0]
	if web.Host != "::1" || web.Port != 22 || web.AutoConnect {
		t.Fatalf("imported session not normalized: %+v", web)
	}

	// Overwriting with another host must not carry the stored password over.
	merged := overwriteSessions(append([]config.Session(nil), existing...), res.duplicates)
	if merged[0].Host != "evil.example" || merged[0].Password != "" {
		t.Fatalf("password kept for a changed host: %+v", merged[0])
	}
	if !merged[0].AutoConnect {
		t.Fatal("auto-connect of the existing session changed")
	}
	if d := describeOverwrite(existing, res.duplicates); !strings.Contains(d, "evil.example") || !strings.Contains(d, "password removed") {
		t.Fatalf("overwrite description: %q", d)
	}

	// Same account: an export without passwords keeps the stored one.
	same := []config.Session{{ID: "prod", Label: "renamed", Host: "DB.example", Port: 22, User: "admin"}}
	merged = overwriteSessions(append([]config.Session(nil), existing...), same)
	if merged[0].Password != "secret" || merged[0].Label != "renamed" {
		t.Fatalf("same account: %+v", merged[0])
	}
}

func TestWritePrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.json")
	// The save dialog creates the file with default permissions first.
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(path, []byte("data"), true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "data" {
		t.Fatalf("content %q", data)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
			t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
		}
	}
}
