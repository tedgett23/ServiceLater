package clone

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The scheduled pull (docs/backup-and-clone.md §2): a UAT or development instance fetches a package of one profile
// from production's API. Production hands out pull keys, each for one profile and good for nothing else; the target
// sends one with each request, and a passphrase of its own choosing that production encrypts the package with, so it
// is never written down unencrypted on either side.

// PullKeyPrefix starts every pull key.
const PullKeyPrefix = "slp_"

// PassphraseHeader carries the passphrase the target wants the package encrypted with.
const PassphraseHeader = "Clone-Passphrase"

// The source's endpoints, under its address.
const (
	SourcePath  = "/api/v1/instance/clone-source"
	PackagePath = "/api/v1/instance/clone-package"
)

var pullKeyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewPullKey makes a key: 256 random bits after the prefix. It returns the key (shown once, never stored), the part
// kept to tell keys apart, and the hash that is stored.
func NewPullKey() (key, shown string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", nil, err
	}
	key = PullKeyPrefix + strings.ToLower(pullKeyEncoding.EncodeToString(b))
	return key, key[:len(PullKeyPrefix)+8], HashPullKey(key), nil
}

// HashPullKey is the stored form of a key (a key is random, so SHA-256 is enough).
func HashPullKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// PullPassphrase is the passphrase a pulled package is encrypted with: derived from this instance's secrets key for
// that one load, so it is never stored and nobody types it.
func PullPassphrase(derived []byte) string { return hex.EncodeToString(derived) }

// SourceInfo is what a source says about itself to a key that may pull from it.
type SourceInfo struct {
	Instance    string `json:"instance"`
	Environment string `json:"environment"`
	// Binary is the source's version, Schema its migration version.
	Binary string `json:"binary"`
	Schema int64  `json:"schema"`
	// Profile is the one profile this key pulls.
	Profile string `json:"profile"`
}

// Source is a clone source as the target sees it.
type Source struct {
	URL, Key string
	// HTTP is the client (nil: one with no timeout, since a package can take a long while; the context ends it).
	HTTP *http.Client
}

func (s Source) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{}
}

func (s Source) get(ctx context.Context, path string, header http.Header) (*http.Response, error) {
	if s.URL == "" || s.Key == "" {
		return nil, errors.New("set the source's address and pull key first")
	}
	u, err := url.Parse(strings.TrimRight(s.URL, "/") + path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = header
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	res, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the source: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		defer func() { _ = res.Body.Close() }()
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body)
		msg := strings.TrimSpace(body.Error)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		switch {
		case res.StatusCode == http.StatusNotFound && msg == "":
			return nil, fmt.Errorf("the source answered 404: is the address a ServiceLater instance recent enough to serve pulls?")
		case msg == "":
			return nil, fmt.Errorf("the source answered %s", res.Status)
		default:
			return nil, fmt.Errorf("the source answered %d: %s", res.StatusCode, msg)
		}
	}
	return res, nil
}

// Info asks the source who it is and which profile the key pulls.
func (s Source) Info(ctx context.Context) (SourceInfo, error) {
	res, err := s.get(ctx, SourcePath, nil)
	if err != nil {
		return SourceInfo{}, err
	}
	defer func() { _ = res.Body.Close() }()
	var out SourceInfo
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
		return SourceInfo{}, fmt.Errorf("the source's answer: %w", err)
	}
	return out, nil
}

// Fetch streams a fresh package of the key's profile into w, encrypted with passphrase. A package cut short does
// not open (its encryption marks the end), so the caller verifies what it wrote.
func (s Source) Fetch(ctx context.Context, passphrase string, w io.Writer) (int64, error) {
	res, err := s.get(ctx, PackagePath, http.Header{PassphraseHeader: {passphrase}})
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	n, err := io.Copy(w, res.Body)
	if err != nil {
		return n, fmt.Errorf("fetching the package: %w", err)
	}
	return n, nil
}
