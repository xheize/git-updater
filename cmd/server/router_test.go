package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/xheize/git-updater/internal/gitManager"
)

func TestGitHubPushWebhookQueuesSyncJob(t *testing.T) {
	store, err := gitManager.NewSQLiteJobStore(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("create job store: %v", err)
	}
	defer store.Close()

	queue := make(chan gitManager.Job, 1)
	app := fiber.New()
	app.Post("/webhook/github", githubSignatureMiddleware("test-secret"), func(c *fiber.Ctx) error {
		return handleGitHubSyncWebhook(c, queue, store, "main")
	})

	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write(body)
	request := httptest.NewRequest("POST", "/webhook/github", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", "delivery-1")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))

	response, err := app.Test(request)
	if err != nil {
		t.Fatalf("send webhook request: %v", err)
	}
	if response.StatusCode != fiber.StatusAccepted {
		t.Fatalf("response status = %d, expected %d", response.StatusCode, fiber.StatusAccepted)
	}

	queued := <-queue
	if queued.Action != gitManager.JobActionSync {
		t.Errorf("queued action = %q, expected %q", queued.Action, gitManager.JobActionSync)
	}
	if queued.Image != "" || queued.Tag != "" {
		t.Errorf("GitHub sync job unexpectedly has image update data: %#v", queued)
	}

	claimed, ok, err := store.Claim(queued.ID)
	if err != nil || !ok {
		t.Fatalf("claim persisted sync job: ok=%v err=%v", ok, err)
	}
	if claimed.Action != gitManager.JobActionSync {
		t.Errorf("persisted action = %q, expected %q", claimed.Action, gitManager.JobActionSync)
	}

	otherBranchBody := []byte(`{"ref":"refs/heads/release"}`)
	mac = hmac.New(sha256.New, []byte("test-secret"))
	mac.Write(otherBranchBody)
	request = httptest.NewRequest("POST", "/webhook/github", bytes.NewReader(otherBranchBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", "delivery-2")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err = app.Test(request)
	if err != nil {
		t.Fatalf("send other-branch webhook request: %v", err)
	}
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("other-branch response status = %d, expected %d", response.StatusCode, fiber.StatusOK)
	}
	select {
	case unexpected := <-queue:
		t.Fatalf("unexpected queued job for other branch: %#v", unexpected)
	default:
	}
	if _, found, err := store.Get("github-delivery-2"); err != nil || found {
		t.Fatalf("other-branch job was persisted: found=%v err=%v", found, err)
	}
}
