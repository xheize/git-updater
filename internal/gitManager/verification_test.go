package gitManager

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/config"
	"github.com/xheize/git-updater/internal/controller"
)

type verificationTransport func(*http.Request) (*http.Response, error)

func (f verificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockVerification(t *testing.T, f verificationTransport) {
	old := verificationHTTP
	verificationHTTP = &http.Client{Transport: f}
	t.Cleanup(func() { verificationHTTP = old })
}

func TestGitHubIdentityAndBranchProtection(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	t.Setenv("CONTROLLER_LOCAL_MODE", "")
	t.Setenv("GITHUB_TOKEN", "provider-token")
	t.Setenv("GITHUB_REPOSITORY_ID", "123")
	if err := g.repo.DeleteRemote("origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/team/gitops.git"}}); err != nil {
		t.Fatal(err)
	}
	protected := false
	mockVerification(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer provider-token" {
			t.Fatal("provider auth missing")
		}
		body := `{"id":123,"full_name":"team/gitops","default_branch":"main","permissions":{"pull":true,"push":true}}`
		if strings.Contains(r.URL.Path, "/branches/") {
			if protected {
				body = `{"protected":true}`
			} else {
				body = `{"protected":false}`
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	id, err := g.verifyRepository(context.Background(), "main")
	if err != nil || !id.Verified || !id.Writable || id.RepositoryID != "123" {
		t.Fatalf("%+v %v", id, err)
	}
	protected = true
	id, err = g.verifyRepository(context.Background(), "main")
	if err != nil || id.Writable || !id.Protected {
		t.Fatalf("%+v %v", id, err)
	}
	t.Setenv("GITHUB_REPOSITORY_ID", "999")
	if _, err := g.verifyRepository(context.Background(), "main"); err == nil {
		t.Fatal("immutable ID mismatch accepted")
	}
	t.Setenv("CONTROLLER_LOCAL_MODE", "true")
	if _, err := g.verifyRepository(context.Background(), "main"); err == nil {
		t.Fatal("local mode bypassed verification for a network remote")
	}
}
func TestRegistryArtifactGate(t *testing.T) {
	t.Setenv("CONTROLLER_LOCAL_MODE", "")
	t.Setenv("REGISTRY_HOSTS", "registry.test")
	t.Setenv("REGISTRY_AUTH_HOST", "registry.test")
	t.Setenv("REGISTRY_BEARER_TOKEN", "test-token")
	status := 200
	digest := "sha256:" + strings.Repeat("a", 64)
	calls := 0
	mockVerification(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "HEAD" || r.URL.String() != "https://registry.test/v2/team/api/manifests/v2" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Docker-Content-Digest": []string{digest}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	g := &gitManager{}
	change := controller.Change{Image: "registry.test/team/api", Tag: "v2"}
	a, err := g.verifyArtifact(context.Background(), change)
	if err != nil || a.Digest != digest {
		t.Fatalf("%+v %v", a, err)
	}
	status = 404
	if _, err := g.verifyArtifact(context.Background(), change); err == nil {
		t.Fatal("missing tag accepted")
	}
	status = 200
	digest = "not-a-digest"
	if _, err := g.verifyArtifact(context.Background(), change); err == nil {
		t.Fatal("invalid digest accepted")
	}
	change.Image = "evil.test/team/api"
	before := calls
	if _, err := g.verifyArtifact(context.Background(), change); err == nil || calls != before {
		t.Fatal("unconfigured registry contacted")
	}
}
