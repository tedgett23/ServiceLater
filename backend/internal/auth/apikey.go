package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// KeyPrefix starts every API key, so a leaked one is easy to recognise (secret scanners match it).
const KeyPrefix = "slk_"

// keyShown is how much of a key is kept in the clear, to tell keys apart: the prefix and eight characters.
const keyShown = len(KeyPrefix) + 8

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrBadKey is any key that does not work: unknown, revoked, expired, or its account deactivated. Callers never
// learn which.
var ErrBadKey = errors.New("invalid API key")

// NewAPIKey makes a key: 256 random bits after the prefix. It returns the key (shown once, never stored), the part
// kept to tell it apart, and the hash that is stored.
func NewAPIKey() (key, shown string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", nil, err
	}
	key = KeyPrefix + strings.ToLower(keyEncoding.EncodeToString(b))
	return key, key[:keyShown], HashAPIKey(key), nil
}

// HashAPIKey is the stored form of a key. A plain SHA-256 is enough: a key is random, not a password.
func HashAPIKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// APIKeys checks keys presented to the API, as the system, before anyone is known.
type APIKeys struct {
	Pool *pgxpool.Pool
}

// Verify returns who a key acts as: its service account, narrowed to the key's scopes (nil: everything the account
// may do). It records the key's use, at most once a minute.
func (k *APIKeys) Verify(ctx context.Context, key string) (Principal, error) {
	if !strings.HasPrefix(key, KeyPrefix) || len(key) > 200 {
		return Principal{}, ErrBadKey
	}
	var p Principal
	err := db.WithSystem(ctx, k.Pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.GetAPIKeyForAuth(ctx, HashAPIKey(key))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadKey
		} else if err != nil {
			return err
		}
		p = Principal{UserID: row.UserID, Role: row.Role, KeyID: row.ID, Scopes: row.Scopes,
			Expires: time.Now().Add(24 * time.Hour)}
		if row.ExpiresAt.Valid {
			p.Expires = row.ExpiresAt.Time
		}
		return q.TouchAPIKey(ctx, row.ID)
	})
	return p, err
}
