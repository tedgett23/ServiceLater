package config

import "testing"

// The live connection limit is a setting now (Admin → Rate limits); the old variable must not stop a server starting.
func TestTheOldLiveConnectionVariableIsIgnored(t *testing.T) {
	t.Setenv("SL_ROLES", "worker")
	t.Setenv("SL_DATABASE_URL", "postgres://x")
	t.Setenv("SL_MAX_LIVE_CONNS", "many")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}
