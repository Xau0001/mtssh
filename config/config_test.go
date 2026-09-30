package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func setup(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	masterKey, salt = nil, nil
}

func storePath(t *testing.T) string {
	t.Helper()
	p, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRoundTrip(t *testing.T) {
	setup(t)
	if Exists() {
		t.Fatal("store should not exist yet")
	}
	got, err := Load("secret")
	if err != nil || len(got) != 0 {
		t.Fatalf("first load = %v, %v; want empty list", got, err)
	}

	want := []Session{{ID: "a", Label: "srv", Host: "h", Port: 22, User: "u", Password: "pw"}}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(storePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, fileMagic) {
		t.Fatal("store not written in current format")
	}
	if bytes.Contains(data, []byte("pw")) {
		t.Fatal("password visible in store")
	}

	masterKey, salt = nil, nil
	got, err = Load("secret")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if _, err := Load("wrong"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: err = %v", err)
	}
}

func TestSaveWhileLocked(t *testing.T) {
	setup(t)
	if err := Save(nil); err == nil {
		t.Fatal("Save without Load should fail")
	}
}

func TestLegacyMigration(t *testing.T) {
	setup(t)
	want := []Session{{ID: "old", Label: "legacy", Host: "h", Port: 2222, User: "u"}}
	plain, _ := json.Marshal(store{Sessions: want})
	h := sha256.Sum256([]byte("secret"))
	enc, err := encrypt(h[:], plain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(storePath(t), enc); err != nil {
		t.Fatal(err)
	}

	got, err := Load("secret")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	data, _ := os.ReadFile(storePath(t))
	if !bytes.HasPrefix(data, fileMagic) {
		t.Fatal("legacy store was not migrated")
	}
	masterKey, salt = nil, nil
	if got, err = Load("secret"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reload after migration = %+v, %v", got, err)
	}
}

func TestLock(t *testing.T) {
	setup(t)
	if err := Lock(); err != nil {
		t.Fatal(err)
	}
	first := lockHandle
	t.Cleanup(func() { first.Close() })

	// A second holder (same effect as a second process) must be refused.
	if err := Lock(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Lock: err = %v, want ErrAlreadyRunning", err)
	}

	// Once the first holder is gone (process exit), locking works again.
	first.Close()
	if err := Lock(); err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	lockHandle.Close()
}

func TestNewerFormat(t *testing.T) {
	setup(t)
	if err := writeFileAtomic(storePath(t), []byte("MTSSH\x03 some future format")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("secret"); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("Load = %v, want ErrNewerFormat", err)
	}
}

func TestNoHome(t *testing.T) {
	for _, home := range []string{"", "relative"} {
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		masterKey, salt = nil, nil
		if _, err := Dir(); !errors.Is(err, ErrNoHome) {
			t.Fatalf("HOME=%q: Dir = %v, want ErrNoHome", home, err)
		}
		if Exists() {
			t.Fatalf("HOME=%q: Exists without a home directory", home)
		}
		if _, err := Load("secret"); !errors.Is(err, ErrNoHome) {
			t.Fatalf("HOME=%q: Load = %v, want ErrNoHome", home, err)
		}
		if err := Lock(); !errors.Is(err, ErrNoHome) {
			t.Fatalf("HOME=%q: Lock = %v, want ErrNoHome", home, err)
		}
	}
}
