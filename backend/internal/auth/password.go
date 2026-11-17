// Package auth implements password login, JWT access tokens, rotating refresh sessions and RBAC middleware.
package auth

import (
	"errors"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"
)

const (
	// DefaultBcryptCost is what passwords are hashed with.
	DefaultBcryptCost = 12
	minPasswordLen    = 8
	maxPasswordLen    = 72 // bcrypt ignores anything beyond 72 bytes
)

var bcryptCost atomic.Int64

func init() { bcryptCost.Store(DefaultBcryptCost) }

// UseFastHashing makes hashing as cheap as bcrypt allows. Only tests call it (dbtest.Pool does): at the default cost
// each hash or check takes about 180 ms, and every test signs people up and in, which was most of the suite's time.
// A stored hash carries its own cost, so checking one follows whatever it was made with.
func UseFastHashing() { bcryptCost.Store(int64(bcrypt.MinCost)) }

var ErrPasswordPolicy = errors.New("password must be 8 to 72 bytes")

func HashPassword(pw string) (string, error) {
	if len(pw) < minPasswordLen || len(pw) > maxPasswordLen {
		return "", ErrPasswordPolicy
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), int(bcryptCost.Load()))
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
		h, _ := bcrypt.GenerateFromPassword([]byte("dummy-password"), int(bcryptCost.Load()))
		dummyHash = string(h)
	})
	_ = CheckPassword(dummyHash, pw)
}
