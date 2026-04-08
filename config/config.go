package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Session holds all data for a saved SSH session
type Session struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	User        string `json:"user"`
	Password    string `json:"password"`   // stored in AES-GCM encrypted file
	KeyPath     string `json:"key_path"`
	UseKey      bool   `json:"use_key"`
	AutoConnect bool   `json:"auto_connect"`
	Group       string `json:"group"`
}

type store struct {
	Sessions []Session `json:"sessions"`
}

var masterKey []byte

// Init derives a 32-byte AES key from a passphrase
func Init(passphrase string) {
	h := sha256.Sum256([]byte(passphrase))
	masterKey = h[:]
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mtputty", "sessions.enc")
}

// Load reads and decrypts the session store from disk
func Load() ([]Session, error) {
	path := configPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Session{}, nil
	}
	if err != nil {
		return nil, err
	}

	plain, err := decrypt(data)
	if err != nil {
		return nil, err
	}

	var s store
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, err
	}
	return s.Sessions, nil
}

// Save encrypts and writes all sessions to disk
func Save(sessions []Session) error {
	s := store{Sessions: sessions}
	plain, err := json.Marshal(s)
	if err != nil {
		return err
	}
	enc, err := encrypt(plain)
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(configPath()), 0700)
	return os.WriteFile(configPath(), enc, 0600)
}

func encrypt(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, data, nil), nil
}

func decrypt(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, data[:ns], data[ns:], nil)
}
