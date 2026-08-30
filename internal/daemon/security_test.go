package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecureHandlerRejectsNonLocalAPIRequests(t *testing.T) {
	handler := new(Server).secureHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		name   string
		host   string
		origin string
		status int
	}{
		{name: "loopback", host: "127.0.0.1:37623", origin: "http://127.0.0.1:37623", status: http.StatusNoContent},
		{name: "localhost", host: "localhost:37623", origin: "http://localhost:5173", status: http.StatusNoContent},
		{name: "remote host", host: "attacker.example", status: http.StatusForbidden},
		{name: "remote origin", host: "127.0.0.1:37623", origin: "https://attacker.example", status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://"+test.host+"/api/projects/app/start", nil)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}
