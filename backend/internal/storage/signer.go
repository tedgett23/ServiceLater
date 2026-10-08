package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Signer makes and checks the short-lived links a browser uploads a part to or downloads a file from when they go
// through the server. They carry no session (a download is a plain link, and a part upload must not carry the sign-in
// token), so the link itself says what it is for and until when, signed with a key derived from the server's token
// secret, which every api server shares.
type Signer struct{ key []byte }

// NewSigner derives the link key from the server's token secret.
func NewSigner(secret string) *Signer {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("servicelater attachment links v1"))
	return &Signer{key: m.Sum(nil)}
}

// Purposes of a link.
const (
	ForPart     = "part"
	ForDownload = "download"
	ForInline   = "inline"
)

func (s *Signer) mac(purpose string, id uuid.UUID, n int, exp int64) []byte {
	m := hmac.New(sha256.New, s.key)
	_, _ = fmt.Fprintf(m, "%s|%s|%d|%d", purpose, id, n, exp)
	return m.Sum(nil)[:20]
}

// Sign makes a token for purpose on attachment id (and part n, else 0), good until exp.
func (s *Signer) Sign(purpose string, id uuid.UUID, n int, exp time.Time) string {
	e := exp.Unix()
	return strconv.FormatInt(e, 36) + "." + base64.RawURLEncoding.EncodeToString(s.mac(purpose, id, n, e))
}

// Verify reports whether token is good for purpose on attachment id (and part n) now.
func (s *Signer) Verify(token, purpose string, id uuid.UUID, n int, now time.Time) bool {
	es, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	e, err := strconv.ParseInt(es, 36, 64)
	if err != nil || now.Unix() > e {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	return err == nil && hmac.Equal(got, s.mac(purpose, id, n, e))
}
