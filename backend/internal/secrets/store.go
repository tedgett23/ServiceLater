package secrets

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

// ErrNotSet means the secret has no value.
var ErrNotSet = errors.New("not set")

// Set encrypts a value and stores it, as the person in q's transaction (app_set_secret checks settings:manage).
func Set(ctx context.Context, q *dbgen.Queries, k *Keyring, name, value string) error {
	s, err := k.Seal(name, value)
	if err != nil {
		return err
	}
	return q.SetSecret(ctx, dbgen.SetSecretParams{Name: name, KeyID: s.KeyID, Nonce: s.Nonce, Ciphertext: s.Ciphertext})
}

// Clear removes a secret, as the person in q's transaction.
func Clear(ctx context.Context, q *dbgen.Queries, name string) error { return q.ClearSecret(ctx, name) }

// Get reads and decrypts a secret. q must be the system's (db.WithSystem): the app role cannot read secrets.
func Get(ctx context.Context, q *dbgen.Queries, k *Keyring, name string) (string, error) {
	row, err := q.GetSecret(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotSet
	}
	if err != nil {
		return "", err
	}
	return k.Open(name, Sealed{KeyID: row.KeyID, Nonce: row.Nonce, Ciphertext: row.Ciphertext})
}

// Rotate re-encrypts every secret not already under the current key, and returns how many it re-encrypted. q must
// be the system's, in one transaction: the rows are locked, so it is all or nothing. The old keys must still be in the
// keyring.
func Rotate(ctx context.Context, q *dbgen.Queries, k *Keyring) (int, error) {
	if !k.Configured() {
		return 0, ErrNoKey
	}
	rows, err := q.ListSecrets(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.KeyID == k.Current() {
			continue
		}
		v, err := k.Open(r.Name, Sealed{KeyID: r.KeyID, Nonce: r.Nonce, Ciphertext: r.Ciphertext})
		if err != nil {
			return 0, err
		}
		s, err := k.Seal(r.Name, v)
		if err != nil {
			return 0, err
		}
		if err := q.ResealSecret(ctx, dbgen.ResealSecretParams{Name: r.Name, KeyID: s.KeyID, Nonce: s.Nonce, Ciphertext: s.Ciphertext}); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}
