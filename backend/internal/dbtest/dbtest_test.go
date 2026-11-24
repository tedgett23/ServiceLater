package dbtest

import "testing"

func TestScratchURLRequiresAnExplicitScratchDatabase(t *testing.T) {
	for dsn, want := range map[string]bool{
		"postgres://localhost/servicelater_test?sslmode=disable": true,
		"postgres://localhost/servicelater":                      false,
		"postgres://localhost":                                   false,
		"postgres://localhost/sl_test?dbname=servicelater":       false,
		"postgres://localhost/sl_test?service=deployment":        false,
	} {
		if got := scratchURL(dsn); got != want {
			t.Errorf("scratchURL(%q) = %v, want %v", dsn, got, want)
		}
	}
}
