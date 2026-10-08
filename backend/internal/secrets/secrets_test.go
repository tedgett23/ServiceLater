package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestSealedValuesOpenOnlyAsTheSecretTheyWereSealedFor(t *testing.T) {
	k, err := NewKeyring(key(1))
	if err != nil {
		t.Fatal(err)
	}
	s, err := k.Seal("email.password", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if s.KeyID != KeyID(key(1)) || bytes.Contains(s.Ciphertext, []byte("hunter2")) {
		t.Fatalf("sealed: %+v", s)
	}
	if got, err := k.Open("email.password", s); err != nil || got != "hunter2" {
		t.Errorf("open: %q %v", got, err)
	}
	if again, _ := k.Seal("email.password", "hunter2"); bytes.Equal(again.Nonce, s.Nonce) || bytes.Equal(again.Ciphertext, s.Ciphertext) {
		t.Error("each seal has its own nonce")
	}
	if _, err := k.Open("slack.token", s); err == nil || !strings.Contains(err.Error(), "belongs to another secret") {
		t.Errorf("moved to another secret: %v", err)
	}
	tampered := s
	tampered.Ciphertext = append([]byte{}, s.Ciphertext...)
	tampered.Ciphertext[0] ^= 1
	if _, err := k.Open("email.password", tampered); err == nil {
		t.Error("a changed ciphertext opens")
	}
}

func TestOldKeysOpenButNeverSeal(t *testing.T) {
	old, _ := NewKeyring(key(1))
	s, _ := old.Seal("x", "v")
	rotated, err := NewKeyring(key(2), key(1))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open("x", s); err != nil || got != "v" {
		t.Errorf("an old key opens: %q %v", got, err)
	}
	if n, _ := rotated.Seal("x", "v"); n.KeyID != KeyID(key(2)) {
		t.Errorf("seals with the current key: %s", n.KeyID)
	}
	fresh, _ := NewKeyring(key(2))
	if _, err := fresh.Open("x", s); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("a key that is gone: %v", err)
	}
	var none Keyring
	if _, err := none.Seal("x", "v"); !errors.Is(err, ErrNoKey) {
		t.Errorf("no key: %v", err)
	}
	if _, err := NewKeyring(key(1)[:16]); err == nil {
		t.Error("a short key is accepted")
	}
}

func TestKeysComeFromTheEnvironmentOrAFile(t *testing.T) {
	gen, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SL_SECRETS_KEY", gen)
	k, err := FromEnv()
	if err != nil || !k.Configured() {
		t.Fatalf("from the variable: %v", err)
	}
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte(gen+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SL_SECRETS_KEY_FILE", file)
	if _, err := FromEnv(); err == nil {
		t.Error("both the variable and the file are accepted")
	}
	t.Setenv("SL_SECRETS_KEY", "")
	if k2, err := FromEnv(); err != nil || k2.Current() != k.Current() {
		t.Errorf("from the file: %v", err)
	}
	t.Setenv("SL_SECRETS_KEY_FILE", "")
	if k3, err := FromEnv(); err != nil || k3.Configured() {
		t.Errorf("none set is no key, not an error: %v", err)
	}
	t.Setenv("SL_SECRETS_KEY", "not base64!")
	if _, err := FromEnv(); err == nil {
		t.Error("a bad key is accepted")
	}
}
