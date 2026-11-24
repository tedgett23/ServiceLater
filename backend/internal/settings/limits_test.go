package settings

import (
	"strings"
	"testing"
)

func TestLimitsAreChecked(t *testing.T) {
	l := DefaultLimits
	if err := l.Check(); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	l.APIPeoplePerMinute, l.WebhookPerMinute = 0, 0
	if err := l.Check(); err != nil {
		t.Errorf("API and webhook limits may be off: %v", err)
	}
	l.SignInAttempts, l.QueriesAtOnce, l.APIKeysPerMinute = 0, 50, 5
	err := l.Check()
	for _, want := range []string{"sign_in_attempts", "queries_at_once", "api_keys_per_minute: API requests a key may make a minute must be between 10 and 1000000, or 0 for no limit"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "api_people") {
		t.Errorf("0 for people is allowed: %v", err)
	}
	back := LimitsFrom(DefaultLimits.Rows())
	if back != DefaultLimits {
		t.Errorf("round trip: %+v", back)
	}
}
