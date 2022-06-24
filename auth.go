package main

import (
	"net/http"
	"strings"
)

func ValidateToken(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if !strings.HasPrefix(token, "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// TODO: Validate token
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
