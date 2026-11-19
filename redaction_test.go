package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestGitHubFailureOmitsReflectedPrivateValues(t *testing.T) {
	f := newFake(t)
	f.routes["/private"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"opaque-test-value 1111222233334444 synthetic@example.test"}`)
	}
	_, err := f.client().get(t.Context(), f.srv.URL+"/private?token=opaque-test-value", new(any))
	if !isStatus(err, http.StatusForbidden) {
		t.Fatalf("lost status: %v", err)
	}
	for _, value := range []string{"opaque-test-value", "1111222233334444", "synthetic@example.test", "/private"} {
		if strings.Contains(err.Error(), value) {
			t.Fatal("GitHub failure leaked private response")
		}
	}
}
