package gitManager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestWorkerOutcomes(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"app.yaml": "image: nginx:old\n"})
	for _, tc := range []struct {
		id, image, file, status string
		outcome                 JobOutcome
	}{
		{"publish", "nginx", "", jobStatusSucceeded, OutcomePublished},
		{"noop", "nginx", "app.yaml", jobStatusSucceeded, OutcomeAlreadySatisfied},
		{"absent", "busybox", "", jobStatusFailed, OutcomeNoMatch},
		{"absent-file", "busybox", "app.yaml", jobStatusFailed, OutcomeNoMatch},
	} {
		info := runWorkTestJob(t, g, Job{ID: tc.id, Image: tc.image, Tag: "new", File: tc.file})
		if info.Status != tc.status || info.Outcome != tc.outcome || info.Attempts != 1 || info.NextAttemptAt != nil {
			t.Fatalf("%s: %+v", tc.id, info)
		}
	}
	g.autoUpdate = false
	info := runWorkTestJob(t, g, Job{ID: "skipped", Image: "nginx", Tag: "v3"})
	if info.Status != jobStatusSkipped || info.Outcome != OutcomeSkippedPolicy {
		t.Fatalf("skip: %+v", info)
	}
	summary, err := g.jobStore.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Counts[jobStatusSucceeded] != 2 || summary.Outcomes["published"] != 1 || summary.Outcomes["already_satisfied"] != 1 || summary.Outcomes["no_match"] != 2 || summary.Counts[jobStatusSkipped] != 1 {
		t.Fatalf("summary: %+v", summary)
	}
}

func TestWorkReindexesFetchedImageOccurrences(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"old.yaml": "image: nginx:old\n"})
	if err := g.buildImageMapping(); err != nil {
		t.Fatal(err)
	}
	remote, err := g.repo.Remote("origin")
	if err != nil {
		t.Fatal(err)
	}
	path := remote.Config().URLs[0]
	origin, err := git.PlainOpen(path)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := origin.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "added.yaml"), []byte("image: nginx:old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("added.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("external addition", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	info := runWorkTestJob(t, g, Job{ID: "index", Image: "nginx", Tag: "new"})
	if info.Outcome != OutcomePublished {
		t.Fatalf("update: %+v", info)
	}
	for _, name := range []string{"old.yaml", "added.yaml"} {
		data, err := os.ReadFile(filepath.Join(g.workspace, name))
		if err != nil || string(data) != "image: nginx:new\n" {
			t.Fatalf("%s: %s %v", name, data, err)
		}
	}
}

func TestIncompleteIndexDoesNotReportNoMatch(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"broken.yaml": "image: [\n"})
	info := runWorkTestJob(t, g, Job{ID: "incomplete", Image: "nginx", Tag: "new"})
	if info.Status != jobStatusRetrying || info.Outcome == OutcomeNoMatch {
		t.Fatalf("incomplete index treated as no match: %+v", info)
	}
}
