package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/servicelater/servicelater/backend/internal/db"
	"github.com/servicelater/servicelater/backend/internal/db/dbgen"
)

const (
	loginMaxAttempts = 10
	loginWindow      = 15 * time.Minute
	// A refresh token presented again shortly after rotation is a parallel request (two tabs), not theft.
	reuseGrace = 10 * time.Second
)

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidSession     = errors.New("invalid or expired session")
	ErrRateLimited        = errors.New("too many attempts, try again later")
)

type User struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Name  string    `json:"name"`
	Role  string    `json:"role"`
}

// Session is what a successful login or refresh yields; the HTTP layer turns it into cookies.
type Session struct {
	User           User
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	RefreshExpires time.Time
}

type Service struct {
	pool       *pgxpool.Pool
	tokens     *Tokens
	refreshTTL time.Duration
}

func NewService(pool *pgxpool.Pool, tokens *Tokens, refreshTTL time.Duration) *Service {
	return &Service{pool: pool, tokens: tokens, refreshTTL: refreshTTL}
}

// Login checks credentials. clientKey (e.g. the remote IP) scopes the rate limit together with the email.
func (s *Service) Login(ctx context.Context, email, password, clientKey string) (*Session, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	key := "login:" + clientKey + ":" + email

	// Count the attempt in its own committed transaction so failed logins still consume the budget.
	var attempts int32
	err := db.WithSystem(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		attempts, err = dbgen.New(tx).HitRateLimit(ctx, dbgen.HitRateLimitParams{Key: key, WindowSecs: loginWindow.Seconds()})
		return err
	})
	if err != nil {
		return nil, err
	}
	if attempts > loginMaxAttempts {
		return nil, ErrRateLimited
	}

	var sess *Session
	var failure error
	err = db.WithSystem(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		u, err := q.GetLoginUser(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			burn(password)
			failure = ErrInvalidCredentials
			return nil
		}
		if err != nil {
			return err
		}
		if !CheckPassword(u.PasswordHash, password) {
			failure = ErrInvalidCredentials
			return nil
		}
		if err := q.ResetRateLimit(ctx, key); err != nil {
			return err
		}
		sess, err = s.issue(ctx, q, User{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role}, uuid.New())
		return err
	})
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, failure
	}
	return sess, nil
}

// Refresh rotates a refresh token. Presenting an already-rotated token outside the grace window revokes the
// whole session family (assume theft).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	var sess *Session
	var failure error
	err := db.WithSystem(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.GetSessionForUpdate(ctx, hashToken(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			failure = ErrInvalidSession
			return nil
		}
		if err != nil {
			return err
		}
		if !row.ExpiresAt.Time.After(time.Now()) {
			failure = ErrInvalidSession
			return nil
		}
		if row.RotatedAt.Valid {
			if time.Since(row.RotatedAt.Time) > reuseGrace {
				if err := q.DeleteSessionFamily(ctx, row.FamilyID); err != nil {
					return err
				}
			}
			failure = ErrInvalidSession
			return nil // commit the revocation
		}
		u, err := q.GetUser(ctx, row.UserID)
		if err != nil {
			return err
		}
		if err := q.MarkSessionRotated(ctx, row.ID); err != nil {
			return err
		}
		sess, err = s.issue(ctx, q, User{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role}, row.FamilyID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, failure
	}
	return sess, nil
}

// Logout revokes the session family of a refresh token. Unknown tokens are ignored.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	return db.WithSystem(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.GetSessionForUpdate(ctx, hashToken(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return q.DeleteSessionFamily(ctx, row.FamilyID)
	})
}

// Bootstrap creates the first admin if no users exist. It reports whether it created one.
func (s *Service) Bootstrap(ctx context.Context, email, name, password string) (bool, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return false, fmt.Errorf("bootstrap admin: %w", err)
	}
	created := false
	err = db.WithSystem(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// Serialise concurrent instances so exactly one creates the admin.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('servicelater.bootstrap'))"); err != nil {
			return err
		}
		n, err := q.CountUsers(ctx)
		if err != nil || n > 0 {
			return err
		}
		u, err := q.CreateUser(ctx, dbgen.CreateUserParams{Email: email, Name: name, Role: "admin"})
		if err != nil {
			return err
		}
		created = true
		return q.UpsertCredentials(ctx, dbgen.UpsertCredentialsParams{UserID: u.ID, PasswordHash: hash})
	})
	return created, err
}

// Cleanup removes expired sessions and stale rate-limit rows.
func (s *Service) Cleanup(ctx context.Context) error {
	return db.WithSystem(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.DeleteExpiredSessions(ctx); err != nil {
			return err
		}
		_, err := q.DeleteStaleRateLimits(ctx)
		return err
	})
}

func (s *Service) issue(ctx context.Context, q *dbgen.Queries, u User, family uuid.UUID) (*Session, error) {
	access, accessExp, err := s.tokens.Issue(u.ID, u.Role)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	refresh := base64.RawURLEncoding.EncodeToString(raw)
	refreshExp := time.Now().Add(s.refreshTTL)
	if err := q.CreateSession(ctx, dbgen.CreateSessionParams{
		UserID: u.ID, FamilyID: family, TokenHash: hashToken(refresh), ExpiresAt: pgTime(refreshExp),
	}); err != nil {
		return nil, err
	}
	return &Session{User: u, AccessToken: access, AccessExpires: accessExp, RefreshToken: refresh, RefreshExpires: refreshExp}, nil
}

func pgTime(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}
