package api

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/servicelater/servicelater/backend/internal/auth"
)

// rateLimit counts each signed-in request against its caller's limit a minute (settings limits.*, Admin → Rate
// limits): an API key's own limit if it has one, else the setting for keys or for people. Over it, the request gets
// 429 with Retry-After; every counted response says where the caller stands in RateLimit-Limit, -Remaining and
// -Reset (seconds). Counting is in memory, shared between servers once a second (internal/ratelimit).
func (h *handlers) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		limits := h.limits.Limits()
		key, limit := "api:user:"+p.UserID.String(), limits.APIPeoplePerMinute
		if p.KeyID != uuid.Nil {
			key, limit = "api:key:"+p.KeyID.String(), limits.APIKeysPerMinute
			if p.KeyLimit != nil {
				limit = *p.KeyLimit
			}
		}
		d := h.limits.Allow(key, limit)
		if d.Limit == 0 {
			next.ServeHTTP(w, r)
			return
		}
		reset := max(1, int(d.Reset.Sub(h.limits.Now()).Seconds()+0.999))
		w.Header().Set("RateLimit-Limit", strconv.Itoa(d.Limit))
		w.Header().Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
		w.Header().Set("RateLimit-Reset", strconv.Itoa(reset))
		if !d.OK {
			w.Header().Set("Retry-After", strconv.Itoa(reset))
			writeError(w, http.StatusTooManyRequests, "too many requests this minute: try again in "+strconv.Itoa(reset)+" seconds")
			return
		}
		next.ServeHTTP(w, r)
	})
}
