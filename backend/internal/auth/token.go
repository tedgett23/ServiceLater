package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const minSecretLen = 32

// Claims are carried in the access JWT. Role is a convenience for fast middleware rejection only; it can be
// stale for up to the access TTL. Postgres RLS always uses the current role from the database.
type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

type Tokens struct {
	// Keys checks API keys; nil means the API accepts only signed-in sessions.
	Keys *APIKeys

	secret []byte
	ttl    time.Duration
}

func NewTokens(secret string, accessTTL time.Duration) (*Tokens, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("auth: JWT secret must be at least %d bytes", minSecretLen)
	}
	return &Tokens{secret: []byte(secret), ttl: accessTTL}, nil
}

func (t *Tokens) Issue(userID uuid.UUID, role string) (string, time.Time, error) {
	exp := time.Now().Add(t.ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	s, err := tok.SignedString(t.secret)
	return s, exp, err
}

// Parse verifies a token and returns who it identifies and when it stops being valid.
func (t *Tokens) Parse(s string) (Principal, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(s, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return Principal{}, err
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return Principal{}, errors.New("auth: bad subject")
	}
	return Principal{UserID: id, Role: c.Role, Expires: c.ExpiresAt.Time}, nil
}
