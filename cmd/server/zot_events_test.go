package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const zotData = `{"name":"team/backend","reference":"v1.2.3","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mediaType":"application/vnd.oci.image.manifest.v1+json"}`

func ceHeaders() map[string]string {
	h := contractAuth()
	h["ce-specversion"] = "1.0"
	h["ce-id"] = "delivery-1"
	h["ce-source"] = "zotregistry.dev"
	h["ce-type"] = "zotregistry.image.updated"
	return h
}
func TestZotCloudEventsAdmission(t *testing.T) {
	t.Setenv("ZOT_REGISTRY_HOST", "registry.example.com")
	app, store, queue := newContractAPI(t)
	h := ceHeaders()
	result := contractRequest(t, app, "POST", "/webhook/zot", zotData, h, 202)
	id := result["jobId"].(string)
	job := <-queue
	if job.Image != "registry.example.com/team/backend" || job.Tag != "v1.2.3" || job.Force {
		t.Fatalf("wrong job: %+v", job)
	}
	if _, found, err := store.Get(id); err != nil || !found {
		t.Fatal("job not persisted", err)
	}
	contractRequest(t, app, "POST", "/webhook/zot", zotData, h, 202)
	envelope := zotCloudEvent{Version: "1.0", ID: "delivery-1", Source: "zotregistry.dev", Type: "zotregistry.image.updated", Data: json.RawMessage(zotData)}
	b, _ := json.Marshal(envelope)
	sh := contractAuth()
	sh["Content-Type"] = "application/cloudevents+json"
	same := contractRequest(t, app, "POST", "/webhook/zot", string(b), sh, 202)
	if same["jobId"] != id || len(queue) != 0 {
		t.Fatal("duplicate enqueued again")
	}
	contractRequest(t, app, "POST", "/webhook/zot", strings.Replace(zotData, "v1.2.3", "v1.2.4", 1), h, 409)
	h["ce-time"] = "2026-10-03T00:00:00Z"
	contractRequest(t, app, "POST", "/webhook/zot", zotData, h, 409)
	delete(h, "ce-time")
	h["ce-source"] = "another-producer"
	other := contractRequest(t, app, "POST", "/webhook/zot", zotData, h, 202)
	if other["jobId"] == id {
		t.Fatal("source scope collision")
	}
}
func TestZotCloudEventsRejectAndIgnore(t *testing.T) {
	for _, kind := range []string{"missing-id", "version", "bad-time", "bad-json", "missing-data", "no-host", "ambiguous-host", "invalid-host", "deleted", "digest", "auth", "mixed", "wrong-content-type"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("ZOT_REGISTRY_HOST", "")
			t.Setenv("REGISTRY_HOSTS", "registry.example.com")
			app, store, _ := newContractAPI(t)
			h := ceHeaders()
			body := zotData
			status := 400
			switch kind {
			case "missing-id":
				delete(h, "ce-id")
			case "version":
				h["ce-specversion"] = "0.3"
			case "bad-time":
				h["ce-time"] = "yesterday"
			case "bad-json":
				body = "{"
			case "missing-data":
				body = "{}"
			case "no-host":
				t.Setenv("REGISTRY_HOSTS", "")
			case "ambiguous-host":
				t.Setenv("REGISTRY_HOSTS", "one.test,two.test")
			case "invalid-host":
				t.Setenv("ZOT_REGISTRY_HOST", "https://bad.test")
			case "deleted":
				h["ce-type"] = "zotregistry.image.deleted"
				status = 200
			case "digest":
				body = strings.Replace(body, "v1.2.3", "sha256:abc", 1)
				status = 200
			case "auth":
				delete(h, "Authorization")
				status = 401
			case "mixed":
				h["Content-Type"] = "application/cloudevents+json"
			case "wrong-content-type":
				h = contractAuth()
				body = `{"specversion":"1.0","id":"x"}`
			}
			contractRequest(t, app, "POST", "/webhook/zot", body, h, status)
			assertNoContractJobs(t, store)
		})
	}
}
func TestZotCloudEventsSingleRegistryFallback(t *testing.T) {
	t.Setenv("ZOT_REGISTRY_HOST", "")
	t.Setenv("REGISTRY_HOSTS", "registry.example.com:5000")
	app, _, queue := newContractAPI(t)
	contractRequest(t, app, "POST", "/webhook/zot", zotData, ceHeaders(), 202)
	if job := <-queue; job.Image != "registry.example.com:5000/team/backend" {
		t.Fatal(job.Image)
	}
}
