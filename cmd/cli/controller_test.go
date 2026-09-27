package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xheize/git-updater/internal/controller"
)

func TestPlanIsPreviewAndApplyUsesOnlyID(t *testing.T) {
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Fatal("authentication missing")
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/changesets" {
			var in controller.Intent
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Fatal(err)
			}
			if len(in.Changes) != 2 || in.Changes[1].Tag != "v3" || in.Changes[0].Environments[0] != "prod" {
				t.Fatalf("%+v", in)
			}
			json.NewEncoder(w).Encode(controller.Plan{ID: in.ID, Intent: in, State: "planned", Atomic: true})
		} else {
			json.NewEncoder(w).Encode(controller.Plan{ID: "release", State: "published", Commit: strings.Repeat("a", 40)})
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	if err := runControllerCLI([]string{"plan", "--server", server.URL, "--key", "test", "--id", "release", "--change", "nginx=v2", "--change", "busybox=v3", "--env", "prod"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !strings.Contains(out.String(), "Preview only") {
		t.Fatal("plan published or did not explain preview")
	}
	if err := runControllerCLI([]string{"apply", "--server", server.URL, "--key", "test", "--id", "release", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != "POST /api/changesets/release/apply" {
		t.Fatalf("%v", calls)
	}
}
func TestCLIErrorsDoNotLookSuccessful(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		w.Write([]byte(`{"code":"plan_conflict"}`))
	}))
	defer server.Close()
	for _, args := range [][]string{{"apply"}, {"plan", "--change", "invalid"}, {"apply", "--id", "old"}, {"unknown"}} {
		args = append(args, "--server", server.URL, "--key", "test")
		var out bytes.Buffer
		if err := runControllerCLI(args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
