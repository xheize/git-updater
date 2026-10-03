package gitManager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
)

func TestAccessProbeCreatesDeletesAndCaches(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	branch, _ := g.BranchName()
	before, _ := g.accessRefs(context.Background())
	if err := g.ensureGitAccess(context.Background(), branch); err != nil {
		t.Fatal(err)
	}
	after, _ := g.accessRefs(context.Background())
	if len(before) != len(after) {
		t.Fatal("probe ref leaked")
	}
	for name, hash := range before {
		if after[name] != hash {
			t.Fatal("existing ref changed")
		}
	}
	checked := g.accessCheckedAt
	if err := g.ensureGitAccess(context.Background(), branch); err != nil || !g.accessCheckedAt.Equal(checked) {
		t.Fatal("success was not cached", err)
	}
	var n int
	g.jobStore.db.QueryRow(`SELECT count(*) FROM git_access_probes`).Scan(&n)
	if n != 0 {
		t.Fatal("pending probe retained")
	}
	g.accessCheckedAt = time.Time{}
	if err := g.ensureGitAccess(context.Background(), "missing"); err == nil {
		t.Fatal("missing target accepted")
	}
}

func TestAccessProbeCrashCleanupAndChangedRef(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	branch, _ := g.BranchName()
	if err := g.ensureGitAccess(context.Background(), branch); err != nil {
		t.Fatal(err)
	}
	raw, _ := g.originURL()
	head, _ := g.repo.Head()
	ref := "refs/heads/git-updater-verify/" + strings.Repeat("a", 32)
	if err := g.repo.Push(&git.PushOptions{RefSpecs: []config.RefSpec{config.RefSpec(head.Hash().String() + ":" + ref)}}); err != nil {
		t.Fatal(err)
	}
	_, err := g.jobStore.db.Exec(`INSERT INTO git_access_probes(origin,ref,revision) VALUES(?,?,?)`, raw, ref, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	g.accessCheckedAt = time.Time{}
	if err := g.ensureGitAccess(context.Background(), branch); err == nil {
		t.Fatal("changed probe deleted")
	}
	refs, _ := g.accessRefs(context.Background())
	if refs[plumbing.ReferenceName(ref)] != head.Hash() {
		t.Fatal("foreign ref modified")
	}
	g.jobStore.db.Exec(`UPDATE git_access_probes SET revision=? WHERE origin=?`, head.Hash().String(), raw)
	if err := g.ensureGitAccess(context.Background(), branch); err != nil {
		t.Fatal(err)
	}
	refs, _ = g.accessRefs(context.Background())
	if _, ok := refs[plumbing.ReferenceName(ref)]; ok {
		t.Fatal("crash leftover not removed")
	}
}

func TestSSHIdentityNeedsProbeAndDoesNotClaimProviderIdentity(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	t.Setenv("CONTROLLER_LOCAL_MODE", "")
	t.Setenv("GIT_AUTH_METHOD", "ssh")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPOSITORY_ID", "")
	g.repo.DeleteRemote("origin")
	g.repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"git@github.com:team/gitops.git"}})
	branch, _ := g.BranchName()
	if _, err := g.verifyRepository(context.Background(), branch); err == nil {
		t.Fatal("unprobed SSH accepted")
	}
	g.accessOrigin = "git@github.com:team/gitops.git"
	g.accessBranch = branch
	g.accessCheckedAt = time.Now()
	id, err := g.verifyRepository(context.Background(), branch)
	if err != nil || !id.Verified || !id.Writable || id.ProtectionKnown || id.Provider != "git-ssh" {
		t.Fatalf("%+v %v", id, err)
	}
	t.Setenv("GITHUB_REPOSITORY_ID", "123")
	if _, err = g.verifyRepository(context.Background(), branch); err == nil {
		t.Fatal("immutable ID pin bypassed")
	}
}

// Explicit opt-in: creates and removes one random remote verification ref.
// Never changes the target branch and never runs in the ordinary test suite.
func TestConfiguredRemoteAccessProbe(t *testing.T) {
	if os.Getenv("RUN_REMOTE_ACCESS_TEST") != "1" {
		t.Skip("explicit remote write-test opt-in required")
	}
	opts, err := getGitAuth()
	if err != nil {
		t.Fatal("Git credentials could not be loaded")
	}
	raw := normalizeGitURL(os.Getenv("GIT_REPOSITORY_URL"), os.Getenv("GIT_AUTH_METHOD"))
	branch := os.Getenv("GIT_TARGET_BRANCH")
	if branch == "" {
		t.Fatal("explicit target branch required")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repo, err := git.PlainCloneContext(ctx, filepath.Join(root, "repo"), &git.CloneOptions{URL: raw, ClientOptions: opts, ReferenceName: plumbing.NewBranchReferenceName(branch)})
	if err != nil {
		t.Fatal("remote clone failed")
	}
	store, err := NewSQLiteJobStore(filepath.Join(root, "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g := &gitManager{repo: repo, repoURL: raw, authOpts: opts, jobStore: store}
	before, err := g.accessRefs(ctx)
	if err != nil {
		t.Fatal("remote ref lookup failed")
	}
	if err = g.ensureGitAccess(ctx, branch); err != nil {
		t.Fatal(err)
	}
	after, err := g.accessRefs(ctx)
	if err != nil {
		t.Fatal("post-probe ref lookup failed")
	}
	if before[plumbing.NewBranchReferenceName(branch)] != after[plumbing.NewBranchReferenceName(branch)] {
		t.Fatal("target branch moved during probe")
	}
	t.Log("Remote read, verification-ref push and deletion succeeded; target branch unchanged")
}
