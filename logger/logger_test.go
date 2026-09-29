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
