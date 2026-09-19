package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
	"github.com/servicelater/servicelater/backend/internal/dbtest"
)

const secret = "0123456789abcdef0123456789abcdef"

func TestPassword(t *testing.T) {
	if _, err := auth.HashPassword("short"); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Errorf("short password: %v", err)
	}
	if _, err := auth.HashPassword(strings.Repeat("a", 73)); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Errorf("long password: %v", err)
	}
	h, err := auth.HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(h, "correct horse") || auth.CheckPassword(h, "wrong horse") {
		t.Error("password check wrong")
	}
}

func TestTokens(t *testing.T) {
	tk, _ := auth.NewTokens(secret, time.Minute)
	id := uuid.New()
	s, _, _ := tk.Issue(id, "agent")
	got, err := tk.Parse(s)
	if err != nil || got.UserID != id || got.Role != "agent" || time.Until(got.Expires) <= 0 {
		t.Fatalf("parse: %+v %v", got, err)
	}
	other, _ := auth.NewTokens(strings.ToUpper(secret), time.Minute)
	if _, err := other.Parse(s); err == nil {
		t.Error("token signed with another key accepted")
	}
	expired, _ := auth.NewTokens(secret, -time.Minute)
	e, _, _ := expired.Issue(id, "agent")
	if _, err := tk.Parse(e); err == nil {
		t.Error("expired token accepted")
	}
	// alg=none must be rejected.
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, auth.Claims{Role: "admin",
		RegisteredClaims: jwt.RegisteredClaims{Subject: id.String(), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := tk.Parse(none); err == nil {
		t.Error("alg=none accepted")
	}
	if _, err := auth.NewTokens("short", time.Minute); err == nil {
		t.Error("short secret accepted")
	}
}

func newService(t *testing.T) *auth.Service {
	pool := dbtest.Pool(t)
	tk, _ := auth.NewTokens(secret, time.Minute)
	svc := auth.NewService(pool, tk, time.Hour)
	if created, err := svc.Bootstrap(context.Background(), "Admin@Example.com", "Admin", "hunter2hunter2"); err != nil || !created {
		t.Fatalf("bootstrap: %v %v", created, err)
	}
	return svc
}

func TestCreateUser(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	u, err := svc.CreateUser(ctx, " Agent@Example.com ", "Ada Agent", "agent", "correct horse")
	if err != nil || u.Email != "agent@example.com" || u.Role != "agent" {
		t.Fatalf("create: %+v %v", u, err)
	}
	if _, err := svc.Login(ctx, "agent@example.com", "correct horse", "ip"); err != nil {
		t.Errorf("new user cannot log in: %v", err)
	}
	if _, err := svc.CreateUser(ctx, "AGENT@example.com", "Dup", "agent", "correct horse"); !errors.Is(err, auth.ErrEmailTaken) {
		t.Errorf("duplicate email: %v", err)
	}
	for name, args := range map[string][4]string{
		"bad role":      {"x@example.com", "X", "superuser", "correct horse"},
		"no @":          {"nope", "X", "agent", "correct horse"},
		"no name":       {"y@example.com", " ", "agent", "correct horse"},
		"weak password": {"z@example.com", "Z", "agent", "short"},
	} {
		if _, err := svc.CreateUser(ctx, args[0], args[1], args[2], args[3]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestBootstrapOnce(t *testing.T) {
	svc := newService(t)
	created, err := svc.Bootstrap(context.Background(), "second@example.com", "Second", "hunter2hunter2")
	if err != nil || created {
		t.Errorf("second bootstrap: created=%v err=%v", created, err)
	}
}

func TestLogin(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	s, err := svc.Login(ctx, " admin@EXAMPLE.com ", "hunter2hunter2", "1.2.3.4")
	if err != nil || s.User.Role != "admin" || s.RefreshToken == "" {
		t.Fatalf("login: %+v %v", s, err)
	}
	if _, err := svc.Login(ctx, "admin@example.com", "nope", "1.2.3.4"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := svc.Login(ctx, "ghost@example.com", "whatever1", "1.2.3.4"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	for i := 0; i < 10; i++ {
		if _, err := svc.Login(ctx, "admin@example.com", "wrong", "9.9.9.9"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Now even the right password is refused for this client+email...
	if _, err := svc.Login(ctx, "admin@example.com", "hunter2hunter2", "9.9.9.9"); !errors.Is(err, auth.ErrRateLimited) {
		t.Errorf("want rate limited, got %v", err)
	}
	// ...but not for another client.
	if _, err := svc.Login(ctx, "admin@example.com", "hunter2hunter2", "8.8.8.8"); err != nil {
		t.Errorf("other client blocked: %v", err)
	}
}

func TestRefreshRotationAndReuse(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	s1, _ := svc.Login(ctx, "admin@example.com", "hunter2hunter2", "ip")

	s2, err := svc.Refresh(ctx, s1.RefreshToken)
	if err != nil || s2.RefreshToken == s1.RefreshToken {
		t.Fatalf("rotate: %v", err)
	}
	// Immediate replay of the old token is treated as a parallel request: rejected, family survives.
	if _, err := svc.Refresh(ctx, s1.RefreshToken); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("replay: %v", err)
	}
	if _, err := svc.Refresh(ctx, s2.RefreshToken); err != nil {
		t.Errorf("family should survive grace-window replay: %v", err)
	}
}

func TestRefreshTheftRevokesFamily(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	tk, _ := auth.NewTokens(secret, time.Minute)
	svc := auth.NewService(pool, tk, time.Hour)
	if _, err := svc.Bootstrap(ctx, "admin@example.com", "Admin", "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	s1, _ := svc.Login(ctx, "admin@example.com", "hunter2hunter2", "ip")
	s2, _ := svc.Refresh(ctx, s1.RefreshToken)

	// Age the rotation beyond the grace window, then replay the old token.
	if _, err := pool.Exec(ctx, "UPDATE sessions SET rotated_at = now() - interval '1 minute' WHERE rotated_at IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, s1.RefreshToken); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := svc.Refresh(ctx, s2.RefreshToken); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("family not revoked after reuse: %v", err)
	}
}

func TestLogoutAndExpiry(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	tk, _ := auth.NewTokens(secret, time.Minute)
	svc := auth.NewService(pool, tk, time.Hour)
	if _, err := svc.Bootstrap(ctx, "admin@example.com", "Admin", "hunter2hunter2"); err != nil {
		t.Fatal(err)
	}

	s, _ := svc.Login(ctx, "admin@example.com", "hunter2hunter2", "ip")
	if err := svc.Logout(ctx, s.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, s.RefreshToken); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("refresh after logout: %v", err)
	}

	s, _ = svc.Login(ctx, "admin@example.com", "hunter2hunter2", "ip")
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, s.RefreshToken); !errors.Is(err, auth.ErrInvalidSession) {
		t.Errorf("expired refresh: %v", err)
	}
	if err := svc.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expired sessions not cleaned: %d", n)
	}
}
