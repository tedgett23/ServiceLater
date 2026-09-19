// Package auth implements password login, JWT access tokens, rotating refresh sessions and RBAC middleware.
package auth

import (
	"errors"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	bcryptCost     = 12
	minPasswordLen = 8
	maxPasswordLen = 72 // bcrypt ignores anything beyond 72 bytes
)

var ErrPasswordPolicy = errors.New("password must be 8 to 72 bytes")

func HashPassword(pw string) (string, error) {
	if len(pw) < minPasswordLen || len(pw) > maxPasswordLen {
		return "", ErrPasswordPolicy
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(h), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// burn spends the same time as a real password check, so unknown emails are indistinguishable from wrong passwords.
func burn(pw string) {
	dummyOnce.Do(func() {
		h, _ := bcrypt.GenerateFromPassword([]byte("dummy-password"), bcryptCost)
		dummyHash = string(h)
	})
	_ = CheckPassword(dummyHash, pw)
}
