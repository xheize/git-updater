package gitManager

import (
	"strings"
	"testing"
)

func TestValidateUpdateJob(t *testing.T) {
	for _, image := range []string{"nginx", "ghcr.io/foo/api", "registry.test:5000/team/api", "[::1]:5000/api"} {
		if err := ValidateUpdateJob(Job{Image: image, Tag: "v1.2.3-rc_1", File: "apps/api.yaml"}); err != nil {
			t.Errorf("valid image %q: %v", image, err)
		}
	}
	for _, job := range []Job{
		{Image: " ", Tag: "v1"}, {Image: "nginx", Tag: " "}, {Image: "nginx", Tag: "bad tag"},
		{Image: "nginx", Tag: "v1\n"}, {Image: "nginx", Tag: strings.Repeat("a", 129)},
		{Image: "nginx:old", Tag: "new"}, {Image: "nginx@sha256:" + strings.Repeat("a", 64), Tag: "v1"},
		{Image: "https://registry.test/api", Tag: "v1"}, {Image: "foo/API", Tag: "v1"},
	} {
		if err := ValidateUpdateJob(job); err == nil {
			t.Errorf("accepted invalid job: %+v", job)
		}
	}
	for _, file := range []string{"../app.yaml", "apps/../app.yaml", "/app.yaml", "C:/app.yaml", "C:app.yaml", "app.yaml:stream", `..\app.yaml`, ".git/config.yaml", "app.txt", "app.yaml\x00", " "} {
		if err := ValidateUpdateJob(Job{Image: "nginx", Tag: "v1", File: file}); err == nil {
			t.Errorf("accepted invalid file %q", file)
		}
	}
}

func TestPersistedInvalidJobRejectedBeforeGit(t *testing.T) {
	g := newWorkTestManager(t, map[string]string{"app.yaml": "image: nginx:old\n"})
	before, _ := g.repo.Head()
	info := runWorkTestJob(t, g, Job{ID: "invalid", File: "app.yaml", Image: "nginx", Tag: "bad tag"})
	after, _ := g.repo.Head()
	if info.Status != jobStatusFailed || info.Attempts != 1 || info.NextAttemptAt != nil || before.Hash() != after.Hash() {
		t.Fatalf("invalid job was retried or mutated Git: %+v", info)
	}
}
