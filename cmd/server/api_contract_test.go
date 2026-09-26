package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/xheize/git-updater/internal/gitManager"
)

// No worker is started: these tests verify that HTTP responses correspond to
// durable admission, not that an accepted request has already been published.
func newContractAPI(t *testing.T) (*fiber.App, *gitManager.JobStore, chan gitManager.Job) {
	t.Helper()
	store, err := gitManager.NewSQLiteJobStore(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	queue := make(chan gitManager.Job, 1)
	app := fiber.New()
	setupRoutes(app, queue, store, "main", serverConfig{
		apiKey: "contract-key", githubEnabled: true, githubSecret: "contract-secret", githubRepositoryID: 123,
	})
	return app, store, queue
}

func contractRequest(t *testing.T, app *fiber.App, method, path, body string, headers map[string]string, want int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatalf("%s %s: response is not JSON: %v", method, path, err)
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s: got %d want %d, response=%v", method, path, res.StatusCode, want, result)
	}
	return result
}

func contractAuth() map[string]string {
	return map[string]string{"Authorization": "Bearer contract-key"}
}

func contractSignature(body string) string {
	mac := hmac.New(sha256.New, []byte("contract-secret"))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func assertNoContractJobs(t *testing.T, store *gitManager.JobStore) {
	t.Helper()
	summary, err := store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for state, count := range summary.Counts {
		if count != 0 {
			t.Fatalf("rejected/ignored request created jobs: %s=%d", state, count)
		}
	}
}

func TestAPIContractAuthentication(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/update"}, {"POST", "/webhook"}, {"POST", "/webhook/zot"},
		{"GET", "/api/status"}, {"GET", "/api/jobs/unknown"}, {"POST", "/api/jobs/unknown/retry"},
	} {
		for _, token := range []string{"", "Bearer wrong"} {
			t.Run(route.method+route.path+"/"+token, func(t *testing.T) {
				app, store, _ := newContractAPI(t)
				contractRequest(t, app, route.method, route.path, `{"image":"nginx","tag":"new"}`,
					map[string]string{"Authorization": token}, 401)
				assertNoContractJobs(t, store)
			})
		}
	}
	for _, headers := range []map[string]string{contractAuth(), {"X-API-Key": "contract-key"}} {
		app, _, _ := newContractAPI(t)
		contractRequest(t, app, "GET", "/api/status", "", headers, 200)
	}
}

func TestAPIContractInvalidUpdatePayload(t *testing.T) {
	for _, route := range []string{"/api/update", "/webhook"} {
		for i, body := range []string{
			`{`, `{}`, `null`, `[]`, `{"image":"nginx"}`, `{"tag":"v2"}`,
			`{"image":123,"tag":"v2"}`, `{"image":"nginx","tag":"v2","timestamp":"invalid"}`,
			`{"image":" ","tag":" "}`, `{"image":"nginx","tag":"bad tag"}`,
			`{"image":"nginx:old","tag":"v2"}`, `{"image":"nginx","tag":"v2","file":"../outside.yaml"}`,
			`{"image":"nginx","tag":"v2","file":"C:\\outside.yaml"}`,
			`{"image":"nginx","tag":"v2","file":"app.yaml:stream"}`,
		} {
			t.Run(fmt.Sprintf("%s/%d", route, i), func(t *testing.T) {
				app, store, _ := newContractAPI(t)
				contractRequest(t, app, "POST", route, body, contractAuth(), 400)
				assertNoContractJobs(t, store)
			})
		}
	}
}

func TestAPIContractDurableAdmissionAndDuplicate(t *testing.T) {
	app, store, queue := newContractAPI(t)
	body := `{"id":"same","image":"nginx","tag":"v2","action":"sync"}`
	for i := 0; i < 2; i++ {
		result := contractRequest(t, app, "POST", "/api/update", body, contractAuth(), 202)
		if result["jobId"] != "same" {
			t.Fatalf("unexpected job ID: %v", result)
		}
	}
	if len(queue) != 1 {
		t.Fatalf("duplicate wakeup: queue length %d", len(queue))
	}
	info, found, err := store.Get("same")
	if err != nil || !found || info.Status != "pending" || info.Job.Timestamp.IsZero() || info.Job.Action != gitManager.JobActionUpdate {
		t.Fatalf("unexpected durable job: %+v, found=%v, err=%v", info, found, err)
	}
	// A full notification channel must not discard an accepted SQLite job.
	result := contractRequest(t, app, "POST", "/webhook", `{"image":"busybox","tag":"v2"}`, contractAuth(), 202)
	id, ok := result["jobId"].(string)
	if !ok || id == "" {
		t.Fatalf("missing generated ID: %v", result)
	}
	if _, found, err := store.Get(id); err != nil || !found {
		t.Fatalf("accepted job lost when channel full: found=%v err=%v", found, err)
	}
	status := contractRequest(t, app, "GET", "/api/jobs/same", "", contractAuth(), 200)
	if status["status"] != "pending" || status["attempts"] != float64(0) {
		t.Fatalf("unexpected status response: %v", status)
	}
	contractRequest(t, app, "POST", "/api/jobs/same/retry", "", contractAuth(), 409)
	contractRequest(t, app, "GET", "/api/jobs/missing", "", contractAuth(), 404)
	contractRequest(t, app, "POST", "/api/jobs/missing/retry", "", contractAuth(), 404)
}

func TestAPIContractConcurrentAdmission(t *testing.T) {
	app, store, queue := newContractAPI(t)
	const requests = 24
	var wg sync.WaitGroup
	errors := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "shared"
			if i%2 == 0 {
				id = fmt.Sprintf("unique-%d", i)
			}
			body := fmt.Sprintf(`{"id":%q,"image":"nginx","tag":"v2"}`, id)
			req := httptest.NewRequest("POST", "/api/update", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer contract-key")
			res, err := app.Test(req, 5000)
			if err != nil {
				errors <- err
				return
			}
			defer res.Body.Close()
			var result map[string]any
			if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
				errors <- err
			} else if res.StatusCode != 202 || result["jobId"] != id {
				errors <- fmt.Errorf("concurrent request %s: status=%d response=%v", id, res.StatusCode, result)
			}
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	summary, err := store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Counts["pending"] != requests/2+1 || len(queue) != 1 {
		t.Fatalf("concurrent admission lost or duplicated work: counts=%v queue=%d", summary.Counts, len(queue))
	}
	for i := 0; i < requests; i += 2 {
		if _, found, err := store.Get(fmt.Sprintf("unique-%d", i)); err != nil || !found {
			t.Fatalf("accepted unique job %d missing: found=%v err=%v", i, found, err)
		}
	}
}

func TestAPIContractGitHubSignatureAndFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, event, body, signature string
		want                         int
	}{
		{"missing signature", "push", `{"ref":"refs/heads/main","repository":{"id":123}}`, "", 401},
		{"wrong algorithm", "push", `{"ref":"refs/heads/main","repository":{"id":123}}`, "sha1=abc", 400},
		{"invalid signature", "push", `{"ref":"refs/heads/main","repository":{"id":123}}`, "sha256=abc", 401},
		{"tampered body", "push", `{"ref":"refs/heads/main","repository":{"id":123}}`, contractSignature(`{"ref":"refs/heads/other","repository":{"id":123}}`), 401},
		{"malformed body", "push", `{`, "sign", 400},
		{"missing ref", "push", `{}`, "sign", 400},
		{"missing identity", "push", `{"ref":"refs/heads/main"}`, "sign", 400},
		{"wrong identity", "push", `{"ref":"refs/heads/main","repository":{"id":999}}`, "sign", 403},
		{"identity is not integer", "push", `{"ref":"refs/heads/main","repository":{"id":123.5}}`, "sign", 400},
		{"other branch", "push", `{"ref":"refs/heads/other","repository":{"id":123}}`, "sign", 200},
		{"other event", "ping", `{}`, "sign", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, store, _ := newContractAPI(t)
			signature := tc.signature
			if signature == "sign" {
				signature = contractSignature(tc.body)
			}
			contractRequest(t, app, "POST", "/webhook/github", tc.body, map[string]string{
				"X-GitHub-Event": tc.event, "X-GitHub-Delivery": "delivery", "X-Hub-Signature-256": signature,
			}, tc.want)
			assertNoContractJobs(t, store)
		})
	}
	app, store, queue := newContractAPI(t)
	body := `{"ref":"refs/heads/main","repository":{"id":123},"image":"ignored","tag":"ignored"}`
	for i := 0; i < 2; i++ {
		contractRequest(t, app, "POST", "/webhook/github", body, map[string]string{
			"X-GitHub-Event": "push", "X-GitHub-Delivery": "duplicate", "X-Hub-Signature-256": contractSignature(body),
		}, 202)
	}
	info, found, err := store.Get(deliveryJobID("github:123", "duplicate"))
	if err != nil || !found || info.Job.Action != gitManager.JobActionSync || info.Job.Image != "" || info.Job.Tag != "" || len(queue) != 1 {
		t.Fatalf("invalid GitHub sync admission: %+v, found=%v err=%v queue=%d", info, found, err, len(queue))
	}
}

func TestAPIContractZotAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"malformed", `{`, 400},
		{"missing repository", `{"action":"push","target":{"tag":"v2"}}`, 400},
		{"missing tag", `{"action":"push","target":{"repository":"demo/api"}}`, 400},
		{"invalid tag", `{"action":"push","target":{"repository":"demo/api","tag":"bad tag"}}`, 400},
		{"invalid host", `{"action":"push","target":{"repository":"demo/api","tag":"v2"},"request":{"host":"https://registry.test"}}`, 400},
		{"pull ignored", `{"action":"pull"}`, 200},
		{"delete ignored", `{"action":"delete"}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, store, _ := newContractAPI(t)
			contractRequest(t, app, "POST", "/webhook/zot", tc.body, contractAuth(), tc.want)
			assertNoContractJobs(t, store)
		})
	}
	for _, host := range []string{"", "registry.test:5000"} {
		t.Run("host="+host, func(t *testing.T) {
			app, store, queue := newContractAPI(t)
			body := fmt.Sprintf(`{"id":"zot-one","action":"push","target":{"repository":"demo/api","tag":"v2"},"request":{"host":%q}}`, host)
			for i := 0; i < 2; i++ {
				contractRequest(t, app, "POST", "/webhook/zot", body, contractAuth(), 202)
			}
			want := "demo/api"
			if host != "" {
				want = host + "/" + want
			}
			info, found, err := store.Get(deliveryJobID("zot:"+host, "zot-one"))
			if err != nil || !found || info.Job.Image != want || info.Job.Tag != "v2" || info.Job.Timestamp.IsZero() || len(queue) != 1 {
				t.Fatalf("invalid Zot job: %+v found=%v err=%v", info, found, err)
			}
		})
	}
}

func TestAPIContractDatabaseOutage(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/update", `{"image":"nginx","tag":"v2"}`, 500},
		{"POST", "/webhook", `{"image":"nginx","tag":"v2"}`, 500},
		{"POST", "/webhook/zot", `{"action":"push","target":{"repository":"nginx","tag":"v2"}}`, 500},
		{"GET", "/api/status", "", 503},
		{"GET", "/api/jobs/missing", "", 500},
		{"POST", "/api/jobs/missing/retry", "", 503},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			app, store, queue := newContractAPI(t)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			result := contractRequest(t, app, tc.method, tc.path, tc.body, contractAuth(), tc.want)
			if tc.path == "/api/jobs/missing/retry" && (result["code"] != "job_store_unavailable" || result["error"] != "job store unavailable") {
				t.Fatalf("unstable or leaked storage error: %v", result)
			}
			if len(queue) != 0 {
				t.Fatal("unpersisted job was queued")
			}
		})
	}
}
