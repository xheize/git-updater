package gitManager

import (
	"context"
	"fmt"
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
	store, err := NewSQLiteJobStore(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &gitManager{repo: clone, workspace: workspace, jobStore: store, autoUpdate: true}
}

func TestCommitAuthorWithoutGitConfig(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprint(custom), func(t *testing.T) {
			t.Setenv("GIT_AUTHOR_NAME", "")
			t.Setenv("GIT_AUTHOR_EMAIL", "")
			name, email := "git-updater", "git-updater@localhost"
			if custom {
				name, email = "Deployment Bot", "bot@example.com"
				t.Setenv("GIT_AUTHOR_NAME", name)
				t.Setenv("GIT_AUTHOR_EMAIL", email)
			}
			g := newWorkTestManager(t, map[string]string{"app.yaml": "image: nginx:old\n"})
			if !g.Work(Job{ID: "author", File: "app.yaml", Image: "nginx", Tag: "new"}) {
				t.Fatal("update failed")
			}
			head, err := g.repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			commit, err := g.repo.CommitObject(head.Hash())
			if err != nil {
				t.Fatal(err)
			}
			if commit.Author.Name != name || commit.Author.Email != email || commit.Committer.Email != email {
				t.Fatalf("unexpected commit identity: %#v", commit)
			}
		})
	}
}

func TestWorkerShutdownPreservesPendingJob(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"app.yaml": "image: nginx:old\n"})
	if _, err := g.jobStore.Enqueue(Job{ID: "pending", Image: "nginx", Tag: "new", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	select {
	case <-g.StartWorker(ctx):
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	info, found, err := g.jobStore.Get("pending")
	if err != nil || !found || info.Status != jobStatusPending || info.Attempts != 0 {
		t.Fatalf("pending job changed: %#v, %v", info, err)
	}
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
