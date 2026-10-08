// Package secrets encrypts the values an instance keeps secret (an SMTP password, later API tokens) before they reach
// the database, and decrypts them only where they are used.
//
// Encryption is AES-256-GCM with a random nonce per value, and the secret's name as additional data, so a ciphertext
// copied onto another secret's row does not decrypt. The key never reaches the database: it comes from SL_SECRETS_KEY
// (32 random bytes, base64) or the file SL_SECRETS_KEY_FILE names, so a dump or a backup alone reveals nothing. Each
// value records which key encrypted it (KeyID, derived from the key), so keys can be rotated: older keys are listed in
// SL_SECRETS_OLD_KEYS (or the file SL_SECRETS_OLD_KEYS_FILE, one per line) until `servicelater secrets rotate` has
// re-encrypted everything under the current one.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeySize is the length of a key: AES-256.
const KeySize = 32

// ErrNoKey means the instance has no key, so secrets can be neither stored nor read.
var ErrNoKey = errors.New("no secrets key is configured: set SL_SECRETS_KEY or SL_SECRETS_KEY_FILE (see `servicelater secrets gen-key`)")

// Keyring is the current key, which encrypts, and older ones, which only decrypt.
type Keyring struct {
	current string // key id
	keys    map[string]cipher.AEAD
}

// Sealed is an encrypted value as it is stored.
type Sealed struct {
	KeyID      string
	Nonce      []byte
	Ciphertext []byte
}

// KeyID names a key without revealing it: the first 8 bytes of its SHA-256, in hex.
func KeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// NewKeyring makes a keyring from the current key and any older ones. A nil current key gives an empty keyring,
// which can only report ErrNoKey.
func NewKeyring(current []byte, old ...[]byte) (*Keyring, error) {
	k := &Keyring{keys: map[string]cipher.AEAD{}}
	if current == nil {
		if len(old) > 0 {
			return nil, errors.New("old secrets keys are set but the current one is not")
		}
		return k, nil
	}
	for i, key := range append([][]byte{current}, old...) {
		if len(key) != KeySize {
			return nil, fmt.Errorf("a secrets key must be %d bytes (base64 of 32 random bytes), key %d is %d", KeySize, i+1, len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		id := KeyID(key)
		if i == 0 {
			k.current = id
		}
		k.keys[id] = aead
	}
	return k, nil
}

// Configured reports whether there is a key to encrypt with.
func (k *Keyring) Configured() bool { return k != nil && k.current != "" }

// Current is the id of the key that encrypts.
func (k *Keyring) Current() string { return k.current }

// Has reports whether the keyring can decrypt values sealed with this key.
func (k *Keyring) Has(keyID string) bool { return k != nil && k.keys[keyID] != nil }

func aad(name string) []byte { return []byte("servicelater/secret/" + name) }

// Seal encrypts a secret's value under the current key.
func (k *Keyring) Seal(name, plaintext string) (Sealed, error) {
	if !k.Configured() {
		return Sealed{}, ErrNoKey
	}
	aead := k.keys[k.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, err
	}
	return Sealed{KeyID: k.current, Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, []byte(plaintext), aad(name))}, nil
}

// Open decrypts a secret's value. It fails when the key that sealed it is not in the keyring, or when the value was
// changed or belongs to another secret.
func (k *Keyring) Open(name string, s Sealed) (string, error) {
	if !k.Configured() {
		return "", ErrNoKey
	}
	aead := k.keys[s.KeyID]
	if aead == nil {
		return "", fmt.Errorf("secret %s was encrypted with key %s, which is not configured (set it in SL_SECRETS_OLD_KEYS, or set the secret again)", name, s.KeyID)
	}
	if len(s.Nonce) != aead.NonceSize() {
		return "", fmt.Errorf("secret %s is damaged", name)
	}
	out, err := aead.Open(nil, s.Nonce, s.Ciphertext, aad(name))
	if err != nil {
		return "", fmt.Errorf("secret %s does not decrypt with key %s: it was changed, or belongs to another secret", name, s.KeyID)
	}
	return string(out), nil
}

// GenerateKey returns a new random key, base64-encoded as SL_SECRETS_KEY takes it.
func GenerateKey() (string, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// ParseKey reads a base64 key (standard or URL alphabet, padding optional).
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(s); err == nil {
			if len(key) != KeySize {
				return nil, fmt.Errorf("a secrets key must be %d bytes once decoded, this is %d", KeySize, len(key))
			}
			return key, nil
		}
	}
	return nil, errors.New("a secrets key must be base64 (see `servicelater secrets gen-key`)")
}

// FromEnv builds the keyring from SL_SECRETS_KEY or SL_SECRETS_KEY_FILE, and SL_SECRETS_OLD_KEYS (comma-separated) or
// SL_SECRETS_OLD_KEYS_FILE (one per line). With no key set it is empty, not an error: secrets are then unavailable.
func FromEnv() (*Keyring, error) {
	cur, err := fromVarOrFile("SL_SECRETS_KEY", "SL_SECRETS_KEY_FILE")
	if err != nil {
		return nil, err
	}
	var current []byte
	if cur != "" {
		if current, err = ParseKey(cur); err != nil {
			return nil, fmt.Errorf("SL_SECRETS_KEY: %w", err)
		}
	}
	olds, err := fromVarOrFile("SL_SECRETS_OLD_KEYS", "SL_SECRETS_OLD_KEYS_FILE")
	if err != nil {
		return nil, err
	}
	var old [][]byte
	for _, s := range strings.FieldsFunc(olds, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if strings.TrimSpace(s) == "" {
			continue
		}
		key, err := ParseKey(s)
		if err != nil {
			return nil, fmt.Errorf("SL_SECRETS_OLD_KEYS: %w", err)
		}
		old = append(old, key)
	}
	return NewKeyring(current, old...)
}

func fromVarOrFile(name, fileVar string) (string, error) {
	v, file := os.Getenv(name), os.Getenv(fileVar)
	switch {
	case v != "" && file != "":
		return "", fmt.Errorf("set %s or %s, not both", name, fileVar)
	case file != "":
		data, err := os.ReadFile(file) //nolint:gosec // the operator names the file
		if err != nil {
			return "", fmt.Errorf("%s: %w", fileVar, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return v, nil
}
