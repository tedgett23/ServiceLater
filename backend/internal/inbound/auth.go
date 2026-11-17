package inbound

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

// DMARC outcomes.
const (
	DMARCPass = "pass" // the sender's domain vouched for it: an aligned DKIM signature, or the trusted server said so
	DMARCFail = "fail" // the domain publishes DMARC and nothing vouched for this message
	DMARCNone = "none" // the domain publishes no DMARC policy, and nothing vouched for it
	// DMARCTempError is a policy that could not be read (DNS trouble): not a failure, not a pass.
	DMARCTempError = "temperror"
)

// Auth is how a message's sender was checked.
type Auth struct {
	DMARC string
	// How says what decided it, for the log ("dkim example.com", "mx.google.com said pass", "no aligned signature").
	How string
}

func (a Auth) String() string { return "dmarc " + a.DMARC + ": " + a.How }

// Verifier checks senders. LookupTXT is DNS (nil: the system's resolver); TrustedAuthServ is the receiving server whose
// Authentication-Results may be believed (empty: none).
type Verifier struct {
	LookupTXT       func(domain string) ([]string, error)
	TrustedAuthServ string
}

// orgDomain is a domain's registrable part (mail.example.co.uk: example.co.uk), for DMARC's relaxed alignment.
func orgDomain(d string) string {
	d = strings.ToLower(strings.TrimSuffix(d, "."))
	if org, err := publicsuffix.EffectiveTLDPlusOne(d); err == nil {
		return org
	}
	return d
}

// Check works out the message's DMARC outcome for its From domain. ServiceLater verifies DKIM signatures itself: one
// that verifies and whose domain aligns (relaxed) with the From domain passes. It cannot check SPF (it does not see
// the connection the message came in on), so mail vouched for by SPF alone passes only through the trusted server's
// Authentication-Results, read from the topmost header (the one the receiving server added last).
func (v Verifier) Check(raw []byte, m *Mail) Auth {
	domain := m.Domain()
	if domain == "" {
		return Auth{DMARC: DMARCFail, How: "no sender address"}
	}
	lookup := v.LookupTXT
	if lookup == nil {
		lookup = net.LookupTXT
	}
	verifications, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{LookupTXT: lookup})
	if err == nil {
		for _, ver := range verifications {
			if ver.Err == nil && orgDomain(ver.Domain) == orgDomain(domain) {
				return Auth{DMARC: DMARCPass, How: "DKIM signature of " + ver.Domain}
			}
		}
	}
	if v.TrustedAuthServ != "" && len(m.AuthResults) > 0 {
		id, results, err := authres.Parse(m.AuthResults[0])
		if err == nil && strings.EqualFold(id, v.TrustedAuthServ) {
			for _, r := range results {
				if d, ok := r.(*authres.DMARCResult); ok && d.Value == authres.ResultPass && (d.From == "" || orgDomain(d.From) == orgDomain(domain)) {
					return Auth{DMARC: DMARCPass, How: id + " said pass"}
				}
			}
		}
	}
	rec, err := dmarc.LookupWithOptions(orgDomain(domain), &dmarc.LookupOptions{LookupTXT: lookup})
	switch {
	case errors.Is(err, dmarc.ErrNoPolicy):
		return Auth{DMARC: DMARCNone, How: domain + " publishes no DMARC policy, and no aligned DKIM signature vouched for it"}
	case err != nil:
		return Auth{DMARC: DMARCTempError, How: fmt.Sprintf("the DMARC policy of %s could not be read: %v", orgDomain(domain), err)}
	}
	return Auth{DMARC: DMARCFail, How: fmt.Sprintf("no aligned DKIM signature (%s publishes p=%s)", orgDomain(domain), rec.Policy)}
}
