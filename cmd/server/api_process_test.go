package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in because this builds and launches real binaries and uses native Git.
// Every commit/push is confined to repositories under t.TempDir().
// RUN_API_E2E=1 go test ./cmd/server -run TestAPIProcessLocalGit -v
func TestAPIProcessLocalGit(t *testing.T) {
	if os.Getenv("RUN_API_E2E") != "1" {
		t.Skip("set RUN_API_E2E=1 to run the real HTTP / SQLite / local Git test")
	}
	root := t.TempDir()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	serverBinary := filepath.Join(root, "api-server"+suffix)
	cliBinary := filepath.Join(root, "api-cli"+suffix)
	processCommand(t, "", "go", "build", "-buildvcs=false", "-o", serverBinary, ".")
	processCommand(t, "", "go", "build", "-buildvcs=false", "-o", cliBinary, "../cli")
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	work := filepath.Join(root, "server")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	processCommand(t, "", "git", "init", "--bare", "--initial-branch=main", origin)
	processCommand(t, "", "git", "init", "--initial-branch=main", seed)
	gitAt := func(path string, args ...string) string {
		return processCommand(t, "", "git", append([]string{"-c", "safe.directory=" + filepath.ToSlash(path), "-C", path}, args...)...)
	}
	for _, name := range []string{"api.yaml", "worker.yaml"} {
		content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + strings.TrimSuffix(name, ".yaml") + "\nspec:\n  template:\n    spec:\n      containers:\n        - name: main\n          image: registry.test/demo/api:v1\n"
		if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitAt(seed, "add", ".")
	gitAt(seed, "-c", "user.name=API Test", "-c", "user.email=api-test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "seed")
	gitAt(seed, "push", origin, "main")
	head := func() string { return strings.TrimSpace(gitAt(origin, "rev-parse", "main")) }
	base := head()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 3 * time.Second}
	request := func(method, path, body string, headers map[string]string, want int) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, baseURL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, res.StatusCode, want, data)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, data)
		}
		return result
	}
	waitJob := func(id, want string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			result := request("GET", "/api/jobs/"+id, "", contractAuth(), 200)
			if result["status"] == want {
				return result
			}
			if result["status"] == "succeeded" || result["status"] == "failed" {
				t.Fatalf("job %s: unexpected terminal result %v", id, result)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("job %s did not reach %s", id, want)
		return nil
	}
	start := func(generation string) func() {
		t.Helper()
		logPath := filepath.Join(root, "server-"+generation+".log")
		log, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(serverBinary)
		cmd.Dir, cmd.Stdout, cmd.Stderr = work, log, log
		cmd.Env = processEnvironment(map[string]string{
			"CONTROLLER_LOCAL_MODE": "true", "API_KEY": "contract-key", "WEBHOOK_SECRET": "", "GITHUB_WEBHOOK_SECRET": "contract-secret",
			"GITHUB_WEBHOOK_ENABLED": "true", "GIT_AUTH_METHOD": "http", "GIT_USERNAME": "local-test",
			"GITHUB_REPOSITORY_ID": "123",
			"GIT_PASSWORD":         "local-test", "GIT_REPOSITORY_URL": filepath.ToSlash(origin), "GIT_REPO_URL": "",
			"PORT": strconv.Itoa(port), "JOB_DB_PATH": filepath.Join(root, "jobs.db"), "AUTO_UPDATE": "true",
			"GIT_AUTHOR_NAME": "API Test", "GIT_AUTHOR_EMAIL": "api-test@example.invalid",
		})
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cmd.Process.Kill() // Only this test's child; also exercises restart durability.
			<-done
			log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(logPath)
				t.Logf("server log:\n%s", data)
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-done:
				done <- err
				t.Fatalf("server exited before readiness: %v", err)
			default:
			}
			res, err := client.Get(baseURL + "/ready")
			if err == nil {
				res.Body.Close()
				if res.StatusCode == 200 {
					return stop
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("server readiness timeout")
		return stop
	}
	stop := start("first")
	request("GET", "/health", "", nil, 200)
	request("GET", "/api/status", "", nil, 401)
	request("POST", "/api/update", "{", contractAuth(), 400)
	request("GET", "/api/jobs/missing", "", contractAuth(), 404)
	request("POST", "/api/update", `{"id":"invalid-tag","image":"registry.test/demo/api","tag":"bad tag"}`, contractAuth(), 400)
	request("GET", "/api/jobs/invalid-tag", "", contractAuth(), 404)
	request("POST", "/api/update", `{"id":"invalid-path","file":"../outside.yaml","image":"registry.test/demo/api","tag":"v2"}`, contractAuth(), 400)
	if head() != base {
		t.Fatal("invalid request changed remote Git")
	}
	body := `{"id":"api-update","image":"registry.test/demo/api","tag":"v2"}`
	request("POST", "/api/update", body, contractAuth(), 202)
	published := waitJob("api-update", "succeeded")
	if published["outcome"] != "published" {
		t.Fatalf("publish outcome: %v", published)
	}
	updated := head()
	if updated == base || strings.TrimSpace(gitAt(origin, "rev-parse", "main^")) != base {
		t.Fatal("API update did not publish exactly one new commit")
	}
	for _, name := range []string{"api.yaml", "worker.yaml"} {
		if !strings.Contains(gitAt(origin, "show", "main:"+name), "registry.test/demo/api:v2") {
			t.Fatalf("remote %s was not updated", name)
		}
	}
	request("POST", "/api/update", body, contractAuth(), 202)
	info := waitJob("api-update", "succeeded")
	if head() != updated || info["attempts"] != float64(1) {
		t.Fatal("duplicate delivery repeated the effect")
	}
	request("POST", "/api/update", `{"id":"api-update","image":"registry.test/demo/api","tag":"v99"}`, contractAuth(), 409)
	if head() != updated {
		t.Fatal("conflicting duplicate changed remote Git")
	}
	request("POST", "/webhook", `{"id":"noop","image":"registry.test/demo/api","tag":"v2"}`, contractAuth(), 202)
	noop := waitJob("noop", "succeeded")
	if noop["outcome"] != "already_satisfied" {
		t.Fatalf("no-op outcome: %v", noop)
	}
	if head() != updated {
		t.Fatal("already-applied update created another commit")
	}
	request("POST", "/api/update", `{"id":"absent","image":"registry.test/absent","tag":"v2"}`, contractAuth(), 202)
	absent := waitJob("absent", "failed")
	if absent["outcome"] != "no_match" || absent["attempts"] != float64(1) || head() != updated {
		t.Fatalf("missing image result: %v", absent)
	}
	processCommand(t, "", cliBinary, "-server", baseURL, "-key", "contract-key", "-image", "registry.test/demo/api", "-tag", "v3")
	deadline := time.Now().Add(10 * time.Second)
	for head() == updated && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(gitAt(origin, "show", "main:api.yaml"), "registry.test/demo/api:v3") {
		t.Fatal("CLI request did not reach the remote")
	}
	zot := `{"id":"zot-update","action":"push","target":{"repository":"demo/api","tag":"v4"},"request":{"host":"registry.test"}}`
	zotResult := request("POST", "/webhook/zot", zot, contractAuth(), 202)
	zotJobID := zotResult["jobId"].(string)
	waitJob(zotJobID, "succeeded")
	if !strings.Contains(gitAt(origin, "show", "main:worker.yaml"), "registry.test/demo/api:v4") {
		t.Fatal("Zot request did not reach the remote")
	}
	beforeSync := head()
	otherRepo := `{"ref":"refs/heads/main","repository":{"id":999}}`
	request("POST", "/webhook/github", otherRepo, map[string]string{
		"X-GitHub-Event": "push", "X-GitHub-Delivery": "wrong-repo", "X-Hub-Signature-256": contractSignature(otherRepo),
	}, 403)
	github := `{"ref":"refs/heads/main","repository":{"id":123}}`
	githubResult := request("POST", "/webhook/github", github, map[string]string{
		"X-GitHub-Event": "push", "X-GitHub-Delivery": "sync", "X-Hub-Signature-256": contractSignature(github),
	}, 202)
	waitJob(githubResult["jobId"].(string), "succeeded")
	if head() != beforeSync {
		t.Fatal("GitHub sync unexpectedly changed Git")
	}
	request("POST", "/api/update", `{"id":"missing-file","file":"missing.yaml","image":"registry.test/demo/api","tag":"v5"}`, contractAuth(), 202)
	failed := waitJob("missing-file", "failed")
	if failed["attempts"] != float64(3) || head() != beforeSync {
		t.Fatalf("invalid failure/retry behavior: %v", failed)
	}
	// A repaired repository invalidates the old intent baseline. Manual retry
	// must not silently approve the new state; a new intent is required.
	gitAt(seed, "fetch", origin, "main")
	gitAt(seed, "reset", "--hard", "FETCH_HEAD")
	if err := os.WriteFile(filepath.Join(seed, "missing.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: repaired\nspec:\n  containers:\n  - name: api\n    image: registry.test/demo/api:v4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(seed, "add", "missing.yaml")
	gitAt(seed, "-c", "user.name=API Test", "-c", "user.email=api-test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "add retry target")
	gitAt(seed, "push", origin, "main")
	request("POST", "/api/jobs/missing-file/retry", "", contractAuth(), 202)
	retried := waitJob("missing-file", "failed")
	if retried["outcome"] != "conflict" {
		t.Fatalf("stale manual retry: %v", retried)
	}
	request("POST", "/api/update", `{"id":"repaired-file","file":"missing.yaml","image":"registry.test/demo/api","tag":"v5"}`, contractAuth(), 202)
	waitJob("repaired-file", "succeeded")
	if !strings.Contains(gitAt(origin, "show", "main:missing.yaml"), ":v5") {
		t.Fatal("new intent did not publish the repaired target")
	}

	// CLI model/preview/apply use the same persisted controller as webhooks.
	cli := func(args ...string) string {
		return processCommand(t, "", cliBinary, append(args, "--server", baseURL, "--key", "contract-key", "--json")...)
	}
	inspect := cli("inspect")
	if !strings.Contains(inspect, "effectiveImage") {
		t.Fatal(inspect)
	}
	beforePlan := head()
	preview := cli("plan", "--id", "cli-reviewed", "--change", "registry.test/demo/api=v6")
	if !strings.Contains(preview, `"state": "planned"`) || head() != beforePlan {
		t.Fatal("preview mutated Git or failed")
	}
	request("POST", "/api/changesets", `{"id":"api-stale","changes":[{"image":"registry.test/demo/api","tag":"v7"}]}`, contractAuth(), 200)
	applied := cli("apply", "--id", "cli-reviewed")
	if !strings.Contains(applied, `"state": "published"`) || head() == beforePlan {
		t.Fatal(applied)
	}
	if strings.TrimSpace(gitAt(origin, "rev-parse", "main^")) != beforePlan {
		t.Fatal("plan did not publish exactly one commit")
	}
	afterApply := head()
	cli("apply", "--id", "cli-reviewed")
	if head() != afterApply {
		t.Fatal("duplicate apply created another commit")
	}
	request("POST", "/api/changesets/api-stale/apply", "", contractAuth(), 409)
	request("GET", "/api/changesets", "", nil, 401)
	request("GET", "/api/jobs?limit=0", "", contractAuth(), 400)
	cli("jobs")
	cli("changesets")
	stop()
	start("restart")
	info = request("GET", "/api/jobs/"+zotJobID, "", contractAuth(), 200)
	if info["status"] != "succeeded" {
		t.Fatalf("completed job did not survive restart: %v", info)
	}
	cli("show", "--id", "cli-reviewed")
	t.Log("PASS: CLI inspect/plan/apply/history, stale approval/retry rejection, real HTTP authentication, API/CLI/Zot -> SQLite -> commit/push, duplicate/no-op, GitHub sync, automatic/manual retry, restart durability")
}

func processCommand(t *testing.T, dir, executable string, args ...string) string {
	t.Helper()
	cmd := exec.Command(executable, args...)
	cmd.Dir = dir
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", executable, args, err, data)
	}
	return string(data)
}

func processEnvironment(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, override := overrides[strings.ToUpper(key)]; !override {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}
