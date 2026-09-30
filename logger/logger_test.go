package logger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveOldLogs(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("mtssh_2020-01-01.log", 40*24*time.Hour)
	recent := write("mtssh_2020-02-01.log", 2*24*time.Hour)
	other := write("notes.txt", 400*24*time.Hour)

	removeOldLogs(dir, time.Now().Add(-retention))

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old log was not removed")
	}
	for _, p := range []string{recent, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should be kept: %v", filepath.Base(p), err)
		}
	}
}

func TestRemoveOldLogsIgnoresGlobInDir(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "ab")
	dir := filepath.Join(base, "a[b]")
	for _, d := range []string{victim, dir} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(victim, "mtssh_2020-01-01.log")
	if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-400 * 24 * time.Hour)
	os.Chtimes(p, old, old)

	removeOldLogs(dir, time.Now())
	if _, err := os.Stat(p); err != nil {
		t.Fatal("log file outside the log directory was removed")
	}
}

func TestClean(t *testing.T) {
	tests := map[string]string{
		"plain host.example":            "plain host.example",
		"umlaut äöü":                    "umlaut äöü",
		"x\n2026/01/01 [prod] INFO  ok": `x\n2026/01/01 [prod] INFO  ok`,
		"\x1b[2Jclear":                  `\x1b[2Jclear`,
		"bidi \u202etxt.exe":            `bidi \u202etxt.exe`,
	}
	for in, want := range tests {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}
