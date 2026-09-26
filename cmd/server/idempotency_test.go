package main

import (
	"fmt"
	"testing"
)

func TestConflictingIDRejectsChangedIntent(t *testing.T) {
	for _, body := range []string{
		`{"id":"same","image":"nginx","tag":"v2"}`,
		`{"id":"same","image":"busybox","tag":"v1"}`,
		`{"id":"same","image":"nginx","tag":"v1","file":"app.yaml"}`,
		`{"id":"same","image":"nginx","tag":"v1","force":true}`,
	} {
		app, store, _ := newContractAPI(t)
		contractRequest(t, app, "POST", "/api/update", `{"id":"same","image":"nginx","tag":"v1"}`, contractAuth(), 202)
		result := contractRequest(t, app, "POST", "/webhook", body, contractAuth(), 409)
		if result["code"] != "idempotency_conflict" {
			t.Fatalf("missing conflict code: %v", result)
		}
		info, _, _ := store.Get("same")
		if info.Job.Image != "nginx" || info.Job.Tag != "v1" || info.Job.Force || info.Job.File != "" {
			t.Fatalf("conflict overwrote job: %+v", info)
		}
	}
}

func TestWebhookIDsAreScopedAndPayloadChecked(t *testing.T) {
	app, store, _ := newContractAPI(t)
	contractRequest(t, app, "POST", "/api/update", `{"id":"same","image":"nginx","tag":"v1"}`, contractAuth(), 202)
	ids := map[string]bool{"same": true}
	for _, host := range []string{"registry.one", "registry.two"} {
		body := fmt.Sprintf(`{"id":"same","action":"push","target":{"repository":"app","tag":"v1","digest":"sha256:one"},"request":{"host":%q}}`, host)
		res := contractRequest(t, app, "POST", "/webhook/zot", body, contractAuth(), 202)
		id := res["jobId"].(string)
		if ids[id] {
			t.Fatal("delivery namespace collision")
		}
		ids[id] = true
		duplicate := contractRequest(t, app, "POST", "/webhook/zot", body, contractAuth(), 202)
		if duplicate["jobId"] != id {
			t.Fatal("duplicate got a new ID")
		}
		changed := fmt.Sprintf(`{"id":"same","action":"push","target":{"repository":"app","tag":"v1","digest":"sha256:two"},"request":{"host":%q}}`, host)
		contractRequest(t, app, "POST", "/webhook/zot", changed, contractAuth(), 409)
		if _, found, err := store.Get(id); err != nil || !found {
			t.Fatalf("missing delivery: %v", err)
		}
	}
}
