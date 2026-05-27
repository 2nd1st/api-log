package api

import (
	"net/http"
	"strings"

	"github.com/leoyun/api-log/internal/admin"
)

// authMW wraps handler in a bearer-token check.
//   Authorization: Bearer <token>
// Missing or wrong → 401 with {"error":"unauthorized"}.
// Constant-time comparison; see admin.CompareConst.
func authMW(expectedToken string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(hdr, prefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		got := strings.TrimPrefix(hdr, prefix)
		if !admin.CompareConst(expectedToken, got) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		handler.ServeHTTP(w, r)
	})
}
