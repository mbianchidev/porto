package daemon

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

func (s *Server) secureHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set(
			"Content-Security-Policy",
			"default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'",
		)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !isLocalRequestHost(r.Host) {
				http.Error(w, "Porto API requests must use a loopback host", http.StatusForbidden)
				return
			}
			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && !isLocalOrigin(origin) {
				http.Error(w, "Porto API requests must come from a loopback origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLocalRequestHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLocalOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return isLocalRequestHost(parsed.Host)
}
