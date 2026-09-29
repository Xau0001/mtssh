package config

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

// Session holds all data for a saved SSH session
type Session struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	User        string `json:"user"`
	Password    string `json:"password"` // stored in AES-GCM encrypted file
	KeyPath     string `json:"key_path"`
	UseKey      bool   `json:"use_key"`
	AutoConnect bool   `json:"auto_connect"`
	Group       string `json:"group"`
}

type store struct {
	Sessions []Session `json:"sessions"`
}

// File layout: magic | salt | nonce | ciphertext. The key is derived from the
// passphrase with Argon2id; magic and salt are authenticated as GCM
// additional data. Files written before this format (nonce | ciphertext,
// key = SHA-256(passphrase)) are still readable and are migrated on load.
var fileMagic = []byte("MTSSH\x02")

const saltSize = 16

// ErrWrongPassphrase is returned by Load when the store cannot be decrypted.
var ErrWrongPassphrase = errors.New("wrong passphrase or corrupted session store")

var (
	masterKey []byte
	salt      []byte
)

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".mtssh", "sessions.enc")
}

// Exists reports whether a session store has been created yet.
func Exists() bool {
	_, err := os.Stat(configPath())
	return err == nil
}

// Load derives the encryption key from passphrase and reads the session store.
// If no store exists yet, it returns an empty list and passphrase becomes the
// one used by all later calls to Save.
func Load(passphrase string) ([]Session, error) {
	data, err := os.ReadFile(configPath())
	if errors.Is(err, os.ErrNotExist) {
		return []Session{}, newKey(passphrase)
	}
	if err != nil {
		return nil, err
	}

	legacy := !bytes.HasPrefix(data, fileMagic)
	var plain []byte
	if legacy {
		h := sha256.Sum256([]byte(passphrase))
		plain, err = decrypt(h[:], data, nil)
	} else {
		hdrLen := len(fileMagic) + saltSize
		if len(data) < hdrLen {
			return nil, ErrWrongPassphrase
		}
		fileSalt := bytes.Clone(data[len(fileMagic):hdrLen])
		key := deriveKey(passphrase, fileSalt)
		if plain, err = decrypt(key, data[hdrLen:], data[:hdrLen]); err == nil {
			masterKey, salt = key, fileSalt
		}
	}
	if err != nil {
		return nil, ErrWrongPassphrase
	}

	var s store
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, err
	}

	if legacy {
		if err := newKey(passphrase); err != nil {
			return nil, err
		}
		// Re-encrypt right away so the weakly keyed file does not linger.
		// If this fails, the next Save writes the new format anyway.
		_ = Save(s.Sessions)
	}
	return s.Sessions, nil
}

// Save encrypts and writes all sessions to disk
func Save(sessions []Session) error {
	if masterKey == nil {
		return errors.New("session store is locked")
	}
	plain, err := json.Marshal(store{Sessions: sessions})
	if err != nil {
		return err
	}
	header := append(bytes.Clone(fileMagic), salt...)
	enc, err := encrypt(masterKey, plain, header)
	if err != nil {
		return err
	}
	return writeFileAtomic(configPath(), append(header, enc...))
}

func newKey(passphrase string) error {
	s := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, s); err != nil {
		return err
	}
	masterKey, salt = deriveKey(passphrase, s), s
	return nil
}

func deriveKey(passphrase string, salt []byte) []byte {
	// Parameters recommended by the x/crypto/argon2 documentation.
	return argon2.IDKey([]byte(passphrase), salt, 1, 64*1024, 4, 32)
}

// writeFileAtomic replaces path via a temp file so a crash mid-write
// cannot leave a truncated (and thus undecryptable) session store behind.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".sessions-*.tmp") // created with mode 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func encrypt(key, data, additional []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, data, additional), nil
}

func decrypt(key, data, additional []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, data[:ns], data[ns:], additional)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
