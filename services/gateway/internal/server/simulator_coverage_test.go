package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestEveryRouteHasASimulatorAction keeps the Next.js simulator complete: every public gateway
// route (except health probes) must be listed in web/lib/ui-actions.json with a UI action, and the
// list must not mention routes that no longer exist.
func TestEveryRouteHasASimulatorAction(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "web", "lib", "ui-actions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var actions []struct{ Method, Route, Action string }
	if err := json.Unmarshal(raw, &actions); err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, a := range actions {
		covered[a.Method+" "+a.Route] = true
	}
	h := New(Deps{Accounts: &fakeAccounts{}, JWTSecret: jwtSecret, BankSecret: []byte("b"), NetworkSecret: []byte("n"), Rejections: &nopRejections{}, DisableStateful: true, DevClock: true})
	routes := map[string]bool{}
	err = chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/healthz" || route == "/readyz" {
			return nil
		}
		routes[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for r := range routes {
		if !covered[r] {
			t.Errorf("route %s has no simulator action in web/lib/ui-actions.json", r)
		}
	}
	for r := range covered {
		if !routes[r] {
			t.Errorf("ui-actions.json lists %s, which the gateway does not serve", r)
		}
	}
}
