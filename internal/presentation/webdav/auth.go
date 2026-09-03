package webdav

import "net/http"

type BasicAuthConfig struct {
	Username string
	Password string
}

func BasicAuth(handler http.Handler, config *BasicAuthConfig) http.Handler {
	if config == nil || config.Username == "" {
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != config.Username || pass != config.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
}
