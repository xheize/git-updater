package gitManager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/xheize/git-updater/internal/controller"
)

func TestChangeSetAtomicPublishPersistenceAndCAS(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"api.yaml": workload("nginx:v1"), "worker.yaml": strings.ReplaceAll(workload("busybox:v1"), "name: api", "name: worker")})
	ctx := context.Background()
	base, _ := g.repo.Head()
	invalid := controller.Intent{ID: "invalid", Changes: []controller.Change{{Image: "nginx", Tag: "v2"}, {Image: "missing", Tag: "v2"}}}
	if _, err := g.Preview(ctx, invalid); err == nil {
		t.Fatal("invalid batch accepted")
	}
	head, _ := g.repo.Head()
	if head.Hash() != base.Hash() {
		t.Fatal("preview mutated Git")
	}
	in := controller.Intent{ID: "release", Changes: []controller.Change{{Image: "nginx", Tag: "v2"}, {Image: "busybox", Tag: "v3"}}}
	p, err := g.Preview(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Diffs) != 2 || len(p.Mutations) != 2 {
		t.Fatalf("%+v", p)
	}
	// Competing approval has the same base. It must never be silently rebased.
	stale, err := g.Preview(ctx, controller.Intent{ID: "stale", Changes: []controller.Change{{Image: "nginx", Tag: "v9"}}})
	if err != nil {
		t.Fatal(err)
	}
	dup, err := g.Preview(ctx, in)
	if err != nil || dup.CreatedAt != p.CreatedAt {
		t.Fatalf("duplicate preview: %v", err)
	}
	changed := in
	changed.Changes = []controller.Change{{Image: "nginx", Tag: "v4"}}
	if _, err := g.Preview(ctx, changed); !errors.Is(err, controller.ErrConflict) {
		t.Fatalf("ID collision: %v", err)
	}
	applied, err := g.ApplyPlan(ctx, p.ID)
	if err != nil || applied.State != "published" {
		t.Fatalf("%+v %v", applied, err)
	}
	commit, err := g.repo.CommitObject(plumbing.NewHash(applied.Commit))
	if err != nil || len(commit.ParentHashes) != 1 || commit.ParentHashes[0] != base.Hash() {
		t.Fatalf("not one atomic commit: %v", err)
	}
	for file, tag := range map[string]string{"api.yaml": "nginx:v2", "worker.yaml": "busybox:v3"} {
		data, _ := os.ReadFile(filepath.Join(g.workspace, file))
		if !strings.Contains(string(data), tag) {
			t.Fatal(string(data))
		}
	}
	if _, err := g.ApplyPlan(ctx, stale.ID); !errors.Is(err, controller.ErrConflict) {
		t.Fatalf("stale plan accepted: %v", err)
	}
	again, err := g.ApplyPlan(ctx, p.ID)
	if err != nil || again.Commit != applied.Commit {
		t.Fatal("reapply generated another commit")
	}
	// Simulate push success followed by a lost response/database update.
	applied.State = "publishing"
	if err := g.jobStore.savePlan(applied, false); err != nil {
		t.Fatal(err)
	}
	again, err = g.ApplyPlan(ctx, p.ID)
	if err != nil || again.State != "published" || again.Commit != applied.Commit {
		t.Fatalf("recovery: %+v %v", again, err)
	}
	// A new connection proves persistence independently of the manager object.
	var dbFile string
	if err := g.jobStore.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&dbFile); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteJobStore(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.GetPlan(p.ID)
	if err != nil || saved.Commit != applied.Commit || saved.State != "published" {
		t.Fatalf("persisted: %+v %v", saved, err)
	}
}

func TestConcurrentApplyPublishesOnlyOnce(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	ctx := context.Background()
	base, _ := g.repo.Head()
	p, err := g.Preview(ctx, controller.Intent{ID: "concurrent", Changes: []controller.Change{{Image: "nginx", Tag: "v2"}}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan controller.Plan, 2)
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p, err := g.ApplyPlan(ctx, p.ID); results <- p; failures <- err }()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	hash := ""
	for p := range results {
		if hash != "" && hash != p.Commit {
			t.Fatal("concurrent apply created different commits")
		}
		hash = p.Commit
	}
	head, _ := g.repo.Head()
	commit, err := g.repo.CommitObject(head.Hash())
	if err != nil || commit.ParentHashes[0] != base.Hash() {
		t.Fatal("not a single commit")
	}
}

func TestEmptyWorkspaceAndMismatchPreservation(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	raw, _ := g.originURL()
	t.Setenv("GIT_AUTH_METHOD", "http")
	t.Setenv("GIT_USERNAME", "local")
	t.Setenv("GIT_PASSWORD", "local")
	t.Setenv("GIT_TARGET_BRANCH", "")
	t.Chdir(t.TempDir())
	if err := os.Mkdir("workspace", 0700); err != nil {
		t.Fatal(err)
	}
	m := New(raw, make(chan Job, 1), g.jobStore)
	if m == nil {
		t.Fatal("precreated empty Docker workspace rejected")
	}
	before, err := os.ReadFile("workspace/a.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if New("https://github.com/other/repo.git", make(chan Job, 1), g.jobStore) != nil {
		t.Fatal("wrong workspace reused")
	}
	after, err := os.ReadFile("workspace/a.yaml")
	if err != nil || string(before) != string(after) {
		t.Fatal("workspace was removed on mismatch")
	}
}

func TestMonorepoScopeAndScopeChanges(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	raw, _ := g.originURL()
	origin, err := git.PlainOpen(raw)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := origin.Worktree()
	if err := os.Mkdir(filepath.Join(raw, "apps"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raw, "apps", "b.yaml"), []byte(workload("busybox:v1")), 0600); err != nil {
		t.Fatal(err)
	}
	wt.Add("apps/b.yaml")
	wt.Commit("add app", &git.CommitOptions{Author: commitAuthor()})
	t.Setenv("GITOPS_PATH", "apps")
	view, err := g.Repository(context.Background())
	if err != nil || len(view.Model.Uses) != 1 || view.Model.Uses[0].Source.File != "apps/b.yaml" {
		t.Fatalf("%+v %v", view, err)
	}
	p, err := g.Preview(context.Background(), controller.Intent{ID: "scoped", Changes: []controller.Change{{Image: "busybox", Tag: "v2"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITOPS_PATH", ".")
	if _, err := g.ApplyPlan(context.Background(), p.ID); !errors.Is(err, controller.ErrConflict) {
		t.Fatalf("scope change did not invalidate approval: %v", err)
	}
}

func TestAutomaticPolicySelectsOnlyExistingAllowedEnvironments(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"base/a.yaml": workload("nginx:v1"), "base/kustomization.yaml": "resources: [a.yaml]\n", "dev/kustomization.yaml": "resources: [../base]\nimages:\n- name: nginx\n  newTag: v2\n", "prod/kustomization.yaml": "resources: [../base]\nimages:\n- name: nginx\n  newTag: v3\n"})
	t.Setenv("AUTOMATION_ENVIRONMENTS", "dev,staging")
	outcome, err := g.work(Job{ID: "automatic", Image: "nginx", Tag: "v4"})
	if err != nil || outcome != OutcomePublished {
		t.Fatalf("%s %v", outcome, err)
	}
	data, _ := os.ReadFile(filepath.Join(g.workspace, "prod", "kustomization.yaml"))
	if !strings.Contains(string(data), "newTag: v3") {
		t.Fatal("production policy bypassed")
	}
	t.Setenv("AUTOMATION_ENVIRONMENTS", "staging")
	outcome, err = g.work(Job{ID: "excluded", Image: "nginx", Tag: "v5"})
	if err != nil || outcome != OutcomeSkippedPolicy {
		t.Fatalf("%s %v", outcome, err)
	}
}

func TestResolvedMutationCannotTargetGitMetadata(t *testing.T) {
	for _, name := range []string{".git/config.yaml", "a/.GIT/hooks.yaml", ".git./config.yaml", ".git /config.yaml"} {
		if _, _, err := resolveWorkspaceFile(t.TempDir(), name); err == nil {
			t.Fatalf("unsafe target accepted: %s", name)
		}
	}
}

func TestOldJobRetryCannotRollbackNewIntent(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	// First attempt fails before a plan is complete, but its base is durable.
	old := Job{ID: "old", Image: "nginx", Tag: "v2", File: "missing.yaml"}
	if _, err := g.work(old); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := g.work(Job{ID: "new", Image: "nginx", Tag: "v3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.work(old); !errors.Is(err, controller.ErrConflict) {
		t.Fatalf("old intent retried on new HEAD: %v", err)
	}
}

func TestPreviewRejectsRemoteChangeAfterBaselineCheck(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	base, err := g.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	// Model a user push between work's baseline check and preview's fetch.
	raw, err := g.originURL()
	if err != nil {
		t.Fatal(err)
	}
	origin, err := git.PlainOpen(raw)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := origin.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raw, "a.yaml"), []byte(workload("nginx:v3")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("a.yaml"); err != nil {
		t.Fatal(err)
	}
	userCommit, err := wt.Commit("user update", &git.CommitOptions{Author: commitAuthor()})
	if err != nil {
		t.Fatal(err)
	}
	in := controller.Intent{ID: "stale-job", Changes: []controller.Change{{Image: "nginx", Tag: "v2"}}}
	if _, err := g.preview(context.Background(), in, base.Hash().String()); !errors.Is(err, controller.ErrConflict) {
		t.Fatalf("job rebased onto user update: %v", err)
	}
	if _, err := g.jobStore.GetPlan(in.ID); !errors.Is(err, controller.ErrNotFound) {
		t.Fatalf("stale plan persisted: %v", err)
	}
	remoteHead, err := origin.Head()
	if err != nil || remoteHead.Hash() != userCommit {
		t.Fatalf("user commit changed: %v", err)
	}
	// A fresh intent may still use the new revision.
	in.ID = "fresh-job"
	p, err := g.preview(context.Background(), in, userCommit.String())
	if err != nil || p.BaseRevision != userCommit.String() {
		t.Fatalf("fresh baseline rejected: %+v %v", p, err)
	}
}

func TestKustomizationFilenamePublish(t *testing.T) {
	for _, name := range []string{"Kustomization", "kustomization.yaml", "kustomization.yml"} {
		t.Run(name, func(t *testing.T) {
			original := workload("nginx:v1")
			g := newWorkTestManager(t, map[string]string{
				"app.yaml": original,
				name:       "resources: [app.yaml]\nimages:\n- name: nginx\n  newTag: v1 # keep\n",
			})
			p, err := g.Preview(context.Background(), controller.Intent{ID: "kustomization", Changes: []controller.Change{{Image: "nginx", Tag: "v2", File: name}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Mutations) != 1 || p.Mutations[0].Location.File != name {
				t.Fatalf("unexpected mutations: %+v", p.Mutations)
			}
			p, err = g.ApplyPlan(context.Background(), p.ID)
			if err != nil || p.State != "published" {
				t.Fatalf("apply: %+v %v", p, err)
			}
			raw, err := g.originURL()
			if err != nil {
				t.Fatal(err)
			}
			origin, err := git.PlainOpen(raw)
			if err != nil {
				t.Fatal(err)
			}
			head, err := origin.Head()
			if err != nil {
				t.Fatal(err)
			}
			commit, err := origin.CommitObject(head.Hash())
			if err != nil {
				t.Fatal(err)
			}
			for file, want := range map[string]string{name: "resources: [app.yaml]\nimages:\n- name: nginx\n  newTag: v2 # keep\n", "app.yaml": original} {
				f, err := commit.File(file)
				if err != nil {
					t.Fatal(err)
				}
				got, err := f.Contents()
				if err != nil || got != want {
					t.Fatalf("published %s: %q, %v", file, got, err)
				}
			}
		})
	}
}

func TestPublishRequiresExactRemoteRefEvenAfterRewind(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	base, _ := g.repo.Head()
	if _, err := g.work(Job{ID: "new", Image: "nginx", Tag: "v2"}); err != nil {
		t.Fatal(err)
	}
	newHead, _ := g.repo.Head()
	raw, _ := g.originURL()
	origin, err := git.PlainOpen(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := origin.Storer.SetReference(plumbing.NewHashReference(base.Name(), base.Hash())); err != nil {
		t.Fatal(err)
	}
	branch := newHead.Name().String()
	// A plain fast-forward would succeed after this rewind. Strict CAS must fail.
	err = g.repo.Push(&git.PushOptions{RefSpecs: []config.RefSpec{config.RefSpec(branch + ":" + branch)}, RequireRemoteRefs: []config.RefSpec{config.RefSpec(newHead.Hash().String() + ":" + branch)}})
	if err == nil {
		t.Fatal("CAS allowed remote rewind")
	}
	remoteHead, _ := origin.Head()
	if remoteHead.Hash() != base.Hash() {
		t.Fatal("failed CAS modified remote")
	}
}
func TestRepositoryBindingAndDeletedBranch(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"a.yaml": workload("nginx:v1")})
	g.repoURL = "https://github.com/wrong/repo.git"
	if _, err := g.Repository(context.Background()); err == nil {
		t.Fatal("origin mismatch accepted")
	}
	g.repoURL = ""
	raw, _ := g.originURL()
	origin, err := git.PlainOpen(raw)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := origin.Head()
	if err := origin.Storer.RemoveReference(head.Name()); err != nil {
		t.Fatal(err)
	}
	if err := g.syncRepository(); err == nil {
		t.Fatal("deleted branch reused stale ref")
	}
}
