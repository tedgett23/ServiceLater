package settings

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// SMSPrefix is what every SMS setting's name starts with, SMSAuthToken the secret that holds the provider's token.
const (
	SMSPrefix    = "sms."
	SMSAuthToken = "sms.auth_token" //nolint:gosec // the name of the secret that holds the token, not a token
)

// DefaultTwilioAPI is Twilio's API. A regional edge (https://api.dublin.ie1.twilio.com) or a compatible provider may
// be set instead.
const DefaultTwilioAPI = "https://api.twilio.com"

// SMS is how this instance sends text messages: Twilio's Messages API, from a number (or a messaging service) of the
// account.
type SMS struct {
	// Enabled turns SMS notifications on. Test messages and number verification can be sent while it is off.
	Enabled bool `json:"enabled"`
	// AccountSID is the Twilio account (AC...); its auth token is a secret.
	AccountSID string `json:"account_sid"`
	// From is the sending number (+15551234567) or a messaging service (MG...).
	From string `json:"from"`
	// APIBase is the API's address; Twilio's unless set.
	APIBase string `json:"api_base"`
}

// DefaultSMS is what each SMS setting is until an admin sets it.
var DefaultSMS = SMS{APIBase: DefaultTwilioAPI}

var (
	accountSIDRE = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
	fromRE       = regexp.MustCompile(`^(\+[1-9][0-9]{7,14}|MG[0-9a-fA-F]{32})$`)
)

// SMSFrom reads the SMS settings from stored rows; what is not stored has its default.
func SMSFrom(stored map[string][]byte) SMS {
	s := DefaultSMS
	for name, dst := range map[string]any{"enabled": &s.Enabled, "account_sid": &s.AccountSID, "from": &s.From, "api_base": &s.APIBase} {
		if raw, ok := stored[SMSPrefix+name]; ok {
			_ = json.Unmarshal(raw, dst)
		}
	}
	return s
}

// Rows are the SMS settings as they are stored.
func (s SMS) Rows() map[string][]byte {
	out := map[string][]byte{}
	for name, v := range map[string]any{"enabled": s.Enabled, "account_sid": s.AccountSID, "from": s.From, "api_base": s.APIBase} {
		raw, _ := json.Marshal(v)
		out[SMSPrefix+name] = raw
	}
	return out
}

// Check normalises the settings and says what is wrong with them. Turning SMS on needs an account and a sender.
func (s *SMS) Check() error {
	s.AccountSID, s.From = strings.TrimSpace(s.AccountSID), strings.TrimSpace(s.From)
	s.APIBase = strings.TrimRight(strings.TrimSpace(s.APIBase), "/")
	if s.APIBase == "" {
		s.APIBase = DefaultTwilioAPI
	}
	u, err := url.Parse(s.APIBase)
	switch {
	case s.AccountSID != "" && !accountSIDRE.MatchString(s.AccountSID):
		return fmt.Errorf("account_sid must be the account's SID, AC and 32 hexadecimal characters")
	case s.From != "" && !fromRE.MatchString(s.From):
		return fmt.Errorf("from must be a number in international form (+15551234567) or a messaging service SID (MG...)")
	case err != nil || u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !Local(u.Hostname()))):
		return fmt.Errorf("api_base must be an https address (http only on this machine)")
	case s.Enabled && (s.AccountSID == "" || s.From == ""):
		return fmt.Errorf("turning SMS on needs an account SID and a sender")
	}
	return nil
}

// TeamsPrefix is what every Teams setting's name starts with.
const TeamsPrefix = "teams."

// Teams is whether notifications post to groups' Microsoft Teams channels. Each channel is a group's webhook (a
// secret, TeamsWebhook), made in Teams with the Workflows app's "Post to a channel when a webhook request is
// received".
type Teams struct {
	Enabled bool `json:"enabled"`
}

// TeamsFrom reads the Teams settings.
func TeamsFrom(stored map[string][]byte) Teams {
	var t Teams
	if raw, ok := stored[TeamsPrefix+"enabled"]; ok {
		_ = json.Unmarshal(raw, &t.Enabled)
	}
	return t
}

// Rows are the Teams settings as they are stored.
func (t Teams) Rows() map[string][]byte {
	raw, _ := json.Marshal(t.Enabled)
	return map[string][]byte{TeamsPrefix + "enabled": raw}
}

// TeamsWebhook is the secret holding a group's Teams channel webhook: anyone with it can post to the channel.
// (app_notify_group_channel looks for the same name.)
func TeamsWebhook(group uuid.UUID) string {
	return "teams.group_" + strings.ReplaceAll(group.String(), "-", "")
}

// CheckTeamsWebhook says what is wrong with a webhook address: it must be https and Microsoft's (a Power Automate or
// Logic Apps trigger, or an old-style Office 365 connector), or something on this machine (a relay, a test server).
func CheckTeamsWebhook(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("the webhook must be the address Teams gave you")
	}
	if Local(u.Hostname()) {
		return nil
	}
	h := strings.ToLower(u.Hostname())
	ms := false
	for _, suffix := range []string{".logic.azure.com", ".powerplatform.com", ".webhook.office.com", ".office.com", ".environment.api.powerplatform.com"} {
		if strings.HasSuffix(h, suffix) {
			ms = true
		}
	}
	if u.Scheme != "https" || !ms {
		return fmt.Errorf("the webhook must be an https address from Teams' Workflows (…logic.azure.com or …powerplatform.com)")
	}
	return nil
}
