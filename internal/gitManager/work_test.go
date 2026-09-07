package gitManager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func newWorkTestManager(t *testing.T, files map[string]string) *gitManager {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	repo, err := git.PlainInit(origin, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(origin, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	clone, err := git.PlainClone(workspace, &git.CloneOptions{URL: origin})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clone.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.User.Name, cfg.User.Email = "Test", "test@example.com"
	if err := clone.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteJobStore(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &gitManager{repo: clone, workspace: workspace, jobStore: store, autoUpdate: true}
}

func runWorkTestJob(t *testing.T, g *gitManager, job Job) JobInfo {
	t.Helper()
	job.Action, job.Timestamp = JobActionUpdate, time.Now()
	if _, err := g.jobStore.Enqueue(job); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := g.jobStore.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	g.processClaimedJob(claimed)
	info, found, err := g.jobStore.Get(job.ID)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	return info
}

func TestWorkFileErrorsAreRetried(t *testing.T) {
	for _, file := range []string{"missing.yaml", "broken.yaml", "directory.yaml"} {
		t.Run(file, func(t *testing.T) {
			g := newWorkTestManager(t, map[string]string{"app.yaml": "image: nginx:old\n", "broken.yaml": "image: [\n"})
			if err := os.Mkdir(filepath.Join(g.workspace, "directory.yaml"), 0700); err != nil {
				t.Fatal(err)
			}
			info := runWorkTestJob(t, g, Job{ID: "bad-file", File: file, Image: "nginx", Tag: "new"})
			if info.Status != jobStatusRetrying || info.NextAttemptAt == nil {
				t.Fatalf("expected retry after file error, got %#v", info)
			}
		})
	}
}

func TestWorkDoesNotCommitPartialUpdate(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"app.yaml": "image: nginx:old\n", "broken.yaml": "image: [\n"})
	g.imageToFiles = map[string][]string{"nginx": {"app.yaml", "broken.yaml"}}
	before, err := g.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	info := runWorkTestJob(t, g, Job{ID: "partial", Image: "nginx", Tag: "new"})
	if info.Status != jobStatusRetrying {
		t.Fatalf("expected retry, got %s", info.Status)
	}
	after, err := g.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if before.Hash() != after.Hash() {
		t.Fatal("partially updated files were committed")
	}
}

func TestWorkDuplicateUpdateSucceedsWithoutCommit(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"app.yaml": "image: nginx:old\n"})
	for _, id := range []string{"first", "duplicate"} {
		before, err := g.repo.Head()
		if err != nil {
			t.Fatal(err)
		}
		info := runWorkTestJob(t, g, Job{ID: id, File: "app.yaml", Image: "nginx", Tag: "new"})
		if info.Status != jobStatusSucceeded {
			t.Fatalf("%s: expected success, got %#v", id, info)
		}
		after, err := g.repo.Head()
		if err != nil {
			t.Fatal(err)
		}
		if id == "duplicate" && before.Hash() != after.Hash() {
			t.Fatal("duplicate update created a commit")
		}
		if id == "first" && before.Hash() == after.Hash() {
			t.Fatal("initial update did not create a commit")
		}
	}
}
