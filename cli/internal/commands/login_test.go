package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// deviceLogin keeps polling through "pending" and returns the approved token.
func TestDeviceLoginPolls(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out any
		switch r.URL.Path {
		case "/api/v1/auth/device/start":
			out = map[string]any{"device_code": "dc", "user_code": "ABCD-EFGH",
				"verification_url": "https://x/activate", "expires_in": 30, "interval": 1}
		case "/api/v1/auth/device/poll":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["device_code"] != "dc" {
				t.Errorf("polled with %q", in["device_code"])
			}
			if polls++; polls < 2 {
				out = map[string]string{"status": "pending"}
			} else {
				out = map[string]string{"status": "approved", "token": "mld_ok"}
			}
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	tok, err := deviceLogin(context.Background(), srv.URL)
	if err != nil || tok != "mld_ok" || polls != 2 {
		t.Fatalf("token=%q err=%v polls=%d", tok, err, polls)
	}
}
