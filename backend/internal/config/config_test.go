package config

import "testing"

func TestLiveConnectionLimitIsReadAndChecked(t *testing.T) {
	t.Setenv("SL_ROLES", "worker")
	t.Setenv("SL_DATABASE_URL", "postgres://x")
	if c, err := Load(); err != nil || c.MaxLiveConns != 10 {
		t.Fatalf("default: %d, %v", c.MaxLiveConns, err)
	}
	t.Setenv("SL_MAX_LIVE_CONNS", "40")
	if c, err := Load(); err != nil || c.MaxLiveConns != 40 {
		t.Fatalf("set: %d, %v", c.MaxLiveConns, err)
	}
	for _, bad := range []string{"0", "1001", "many", "-3"} {
		t.Setenv("SL_MAX_LIVE_CONNS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
