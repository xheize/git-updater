package gitManager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	"github.com/xheize/git-updater/internal/controller"
)

func verificationClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var verificationHTTP = verificationClient()

func (g *gitManager) originURL() (string, error) {
	remote, err := g.repo.Remote("origin")
	if err != nil {
		return "", err
	}
	cfg := remote.Config()
	if len(cfg.URLs) != 1 {
		return "", fmt.Errorf("origin must have exactly one URL")
	}
	if g.repoURL != "" && cfg.URLs[0] != g.repoURL {
		return "", fmt.Errorf("configured repository does not match workspace origin")
	}
	return cfg.URLs[0], nil
}
func (g *gitManager) verifyRepository(ctx context.Context, branch string) (controller.Identity, error) {
	id := controller.Identity{Provider: "unverified", Detail: "Only GitHub and explicit local test mode are supported"}
	raw, err := g.originURL()
	if err != nil {
		return id, err
	}
	if os.Getenv("CONTROLLER_LOCAL_MODE") == "true" {
		// Test mode cannot be used to bypass verification for network remotes.
		abs, err := filepath.Abs(raw)
		if err != nil || !filepath.IsAbs(raw) || strings.Contains(raw, "://") || strings.HasPrefix(raw, "\\\\") {
			return id, fmt.Errorf("local mode requires an absolute local filesystem origin")
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return id, err
		}
		sum := sha256.Sum256([]byte(filepath.Clean(real)))
		return controller.Identity{Provider: "local-test", RepositoryID: hex.EncodeToString(sum[:]), Name: filepath.Base(real), DefaultBranch: branch, Verified: true, Writable: true, Detail: "Local test mode; provider and registry checks bypassed"}, nil
	}
	canonical := normalizeGitURL(raw, "http")
	u, err := url.Parse(canonical)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return id, fmt.Errorf("provider verification requires a github.com repository without URL credentials")
	}
	full := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if len(strings.Split(full, "/")) != 2 {
		return id, fmt.Errorf("invalid GitHub repository URL")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" && os.Getenv("GIT_AUTH_METHOD") == "http" {
		token = os.Getenv("GIT_PASSWORD")
	}
	if token == "" {
		return id, fmt.Errorf("GITHUB_TOKEN is required for repository verification")
	}
	get := func(endpoint string, out any) error {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com"+endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		res, err := verificationHTTP.Do(req)
		if err != nil {
			return fmt.Errorf("GitHub verification unavailable")
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("GitHub verification returned HTTP %d", res.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(out)
	}
	var repo struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Archived      bool   `json:"archived"`
		Disabled      bool   `json:"disabled"`
		Permissions   struct {
			Pull bool `json:"pull"`
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := get("/repos/"+full, &repo); err != nil {
		return id, err
	}
	if repo.ID <= 0 || !repo.Permissions.Pull {
		return id, fmt.Errorf("repository identity/read permission unavailable")
	}
	if expected := os.Getenv("GITHUB_REPOSITORY_ID"); expected != "" && expected != strconv.FormatInt(repo.ID, 10) {
		return id, fmt.Errorf("GitHub immutable repository ID mismatch")
	}
	var ref struct {
		Protected bool `json:"protected"`
	}
	if err := get("/repos/"+repo.FullName+"/branches/"+url.PathEscape(branch), &ref); err != nil {
		return id, err
	}
	id = controller.Identity{Provider: "github", RepositoryID: strconv.FormatInt(repo.ID, 10), Name: repo.FullName, DefaultBranch: repo.DefaultBranch, Verified: true, Writable: repo.Permissions.Push && !repo.Archived && !repo.Disabled && !ref.Protected, Protected: ref.Protected, Detail: "Provider identity/access verified; protected branches require a future PR workflow"}
	return id, nil
}
func (g *gitManager) verifyArtifact(ctx context.Context, c controller.Change) (controller.Artifact, error) {
	out := controller.Artifact{Image: c.Image, Tag: c.Tag}
	if os.Getenv("CONTROLLER_LOCAL_MODE") == "true" {
		out.Digest = "unchecked-local-test"
		return out, nil
	}
	n, err := reference.ParseNormalizedNamed(c.Image)
	if err != nil {
		return out, err
	}
	host := reference.Domain(n)
	allowed := false
	for _, h := range strings.Split(os.Getenv("REGISTRY_HOSTS"), ",") {
		if strings.TrimSpace(h) == host {
			allowed = true
		}
	}
	if !allowed {
		return out, fmt.Errorf("registry %s is not in REGISTRY_HOSTS", host)
	}
	endpointHost := host
	if host == "docker.io" {
		endpointHost = "registry-1.docker.io"
	}
	req, err := http.NewRequestWithContext(ctx, "HEAD", "https://"+endpointHost+"/v2/"+reference.Path(n)+"/manifests/"+url.PathEscape(c.Tag), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json")
	if os.Getenv("REGISTRY_AUTH_HOST") == host {
		if token := os.Getenv("REGISTRY_BEARER_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else if user := os.Getenv("REGISTRY_USERNAME"); user != "" {
			req.SetBasicAuth(user, os.Getenv("REGISTRY_PASSWORD"))
		}
	}
	res, err := verificationHTTP.Do(req)
	if err != nil {
		return out, fmt.Errorf("registry verification unavailable for %s", host)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return out, fmt.Errorf("registry %s returned HTTP %d (configure registry credentials if required)", host, res.StatusCode)
	}
	out.Digest = res.Header.Get("Docker-Content-Digest")
	if err := digest.Digest(out.Digest).Validate(); err != nil {
		return out, fmt.Errorf("registry did not return a valid manifest digest")
	}
	return out, nil
}
