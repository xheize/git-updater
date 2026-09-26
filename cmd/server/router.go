package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/xheize/git-updater/internal/gitManager"
)

// setupRoutes registers all route handlers grouped by API and Webhooks
func setupRoutes(app *fiber.App, jobQueue chan gitManager.Job, jobStore *gitManager.JobStore, targetBranch string, cfg serverConfig) {

	// Auth Middleware for standard API and Webhook endpoints
	auth := authMiddleware(cfg.apiKey)

	// Webhook Group
	webhooks := app.Group("/webhook")

	// Standard webhook (API key authenticated, used by CLI)
	webhooks.Post("", auth, func(c *fiber.Ctx) error {
		return handleJobEnqueue(c, jobQueue, jobStore, "webhook")
	})

	// GitHub webhook (GitHub signature verified, GITHUB_WEBHOOK_SECRET key check)
	if cfg.githubEnabled {
		webhooks.Post("/github", githubSignatureMiddleware(cfg.githubSecret), func(c *fiber.Ctx) error {
			return handleGitHubSyncWebhook(c, jobQueue, jobStore, targetBranch, cfg.githubRepositoryID)
		})
	}

	// Zot webhook (API key authenticated, parses Zot events extension JSON format)
	webhooks.Post("/zot", auth, func(c *fiber.Ctx) error {
		return handleZotWebhook(c, jobQueue, jobStore)
	})

	// API Group
	api := app.Group("/api")
	api.Get("/status", auth, func(c *fiber.Ctx) error {
		status, err := jobStore.Summary(c.UserContext())
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "job store unavailable"})
		}
		return c.JSON(status)
	})
	api.Post("/update", auth, func(c *fiber.Ctx) error {
		return handleJobEnqueue(c, jobQueue, jobStore, "api")
	})
	api.Get("/jobs/:id", auth, func(c *fiber.Ctx) error {
		return handleJobStatus(c, jobStore)
	})
	api.Post("/jobs/:id/retry", auth, func(c *fiber.Ctx) error {
		return handleJobRetry(c, jobQueue, jobStore)
	})
}

// handleJobEnqueue validates the request body, sets default ID/Timestamp, and enqueues the job
func handleJobEnqueue(c *fiber.Ctx, jobQueue chan gitManager.Job, jobStore *gitManager.JobStore, source string) error {
	var job gitManager.Job
	if err := c.BodyParser(&job); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "failed to parse request body: " + err.Error(),
		})
	}

	if err := gitManager.ValidateUpdateJob(job); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": err.Error(), "code": "invalid_update",
		})
	}

	if job.ID == "" {
		job.ID = fmt.Sprintf("%s-%d", source, time.Now().UnixNano())
	}
	if job.Timestamp.IsZero() {
		job.Timestamp = time.Now()
	}
	job.Action = gitManager.JobActionUpdate

	inserted, err := jobStore.EnqueueScoped(job, "update", "")
	if err != nil {
		if errors.Is(err, gitManager.ErrIdempotencyConflict) {
			return idempotencyConflict(c)
		}
		log.Printf("Failed to persist job %s: %v", job.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to persist update job",
		})
	}

	if inserted {
		select {
		case jobQueue <- job:
		default:
			// The worker will claim this persisted job after its current work.
		}
	}

	targetMsg := job.File
	if targetMsg == "" {
		targetMsg = job.Image
	}

	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"status":  "accepted",
		"message": "persisted update job for " + targetMsg,
		"jobId":   job.ID,
	})
}

// handleGitHubSyncWebhook queues a workspace synchronization for a repository
// push. GitHub events are not image-update requests and never accept image or
// tag fields from the webhook body.
func handleGitHubSyncWebhook(c *fiber.Ctx, jobQueue chan gitManager.Job, jobStore *gitManager.JobStore, targetBranch string, repositoryID int64) error {
	if c.Get("X-GitHub-Event") != "push" {
		return c.JSON(fiber.Map{
			"status":  "ignored",
			"message": "only GitHub push events trigger workspace synchronization",
		})
	}

	var payload struct {
		Ref        string `json:"ref"`
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
	}
	if err := c.BodyParser(&payload); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "failed to parse GitHub push payload: " + err.Error(),
		})
	}
	if payload.Ref == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "GitHub push payload is missing ref",
		})
	}
	if repositoryID <= 0 {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "GitHub repository identity is not configured", "code": "repository_unconfigured"})
	}
	if payload.Repository.ID <= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "GitHub repository ID is required", "code": "repository_missing"})
	}
	if payload.Repository.ID != repositoryID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "GitHub repository does not match configured identity", "code": "repository_mismatch"})
	}
	if payload.Ref != "refs/heads/"+targetBranch {
		return c.JSON(fiber.Map{
			"status":  "ignored",
			"message": "push ref does not match tracked branch " + targetBranch,
		})
	}

	deliveryID := c.Get("X-GitHub-Delivery")
	if deliveryID == "" {
		deliveryID = fmt.Sprintf("generated-%d", time.Now().UnixNano())
	}
	scope := fmt.Sprintf("github:%d", repositoryID)
	job := gitManager.Job{
		ID:        deliveryJobID(scope, deliveryID),
		Action:    gitManager.JobActionSync,
		Timestamp: time.Now(),
	}

	inserted, err := jobStore.EnqueueScoped(job, scope, webhookPayloadHash(c.Body()))
	if err != nil {
		if errors.Is(err, gitManager.ErrIdempotencyConflict) {
			return idempotencyConflict(c)
		}
		log.Printf("Failed to persist GitHub sync job %s: %v", job.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to persist GitHub synchronization job",
		})
	}
	if inserted {
		select {
		case jobQueue <- job:
		default:
			// The worker will claim this persisted job after its current work.
		}
	}

	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"status":  "accepted",
		"message": "persisted workspace synchronization for " + payload.Ref,
		"jobId":   job.ID,
	})
}

func handleJobStatus(c *fiber.Ctx, jobStore *gitManager.JobStore) error {
	info, found, err := jobStore.Get(c.Params("id"))
	if err != nil {
		log.Printf("Failed to read job %s: %v", c.Params("id"), err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to read job status"})
	}
	if !found {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "job not found"})
	}
	return c.JSON(info)
}

func handleJobRetry(c *fiber.Ctx, jobQueue chan gitManager.Job, jobStore *gitManager.JobStore) error {
	job, found, err := jobStore.Retry(c.Params("id"))
	if err != nil {
		if errors.Is(err, gitManager.ErrJobStateConflict) {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "only failed jobs can be retried", "code": "job_state_conflict"})
		}
		log.Printf("Failed to retry job %s: %v", c.Params("id"), err)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "job store unavailable", "code": "job_store_unavailable"})
	}
	if !found {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "job not found"})
	}
	select {
	case jobQueue <- job:
	default:
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"status":  "accepted",
		"message": "job retry scheduled",
		"jobId":   job.ID,
	})
}

// authMiddleware validates custom API token or Bearer token header
func authMiddleware(apiKey string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if apiKey == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "API authentication is not configured"})
		}

		authHeader := c.Get("Authorization")
		token := ""
		if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			token = authHeader[7:]
		}

		if token == "" {
			token = c.Get("X-API-Key")
		}

		if token != apiKey {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "unauthorized: invalid or missing API key",
			})
		}
		return c.Next()
	}
}

// githubSignatureMiddleware validates GitHub webhook X-Hub-Signature-256 HMAC-SHA256 signature
func githubSignatureMiddleware(secret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if secret == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "GitHub signature verification is not configured"})
		}

		signatureHeader := c.Get("X-Hub-Signature-256")
		if signatureHeader == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "unauthorized: missing X-Hub-Signature-256 header",
			})
		}

		if !strings.HasPrefix(signatureHeader, "sha256=") {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "bad request: invalid signature format, expected sha256=...",
			})
		}
		actualSignature := signatureHeader[7:]

		body := c.Body()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expectedMAC := mac.Sum(nil)
		expectedSignature := hex.EncodeToString(expectedMAC)

		if subtle.ConstantTimeCompare([]byte(actualSignature), []byte(expectedSignature)) != 1 {
			log.Println("WARNING: GitHub webhook signature validation failed.")
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "unauthorized: invalid webhook signature",
			})
		}

		return c.Next()
	}
}

// ZotWebhookPayload represents the OCI Event format payload sent by Zot registry HTTP events sink
type ZotWebhookPayload struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Target    struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
		MediaType  string `json:"mediaType"`
	} `json:"target"`
	Request struct {
		Host string `json:"host"`
	} `json:"request"`
}

// handleZotWebhook validates Zot push notification, maps it into Job, and enqueues it
func handleZotWebhook(c *fiber.Ctx, jobQueue chan gitManager.Job, jobStore *gitManager.JobStore) error {
	var payload ZotWebhookPayload
	if err := c.BodyParser(&payload); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "failed to parse request body: " + err.Error(),
		})
	}

	// We only process "push" events (ignoring pull, delete, etc.)
	if payload.Action != "push" {
		return c.JSON(fiber.Map{
			"status":  "ignored",
			"message": "action " + payload.Action + " is ignored",
		})
	}

	if payload.Target.Repository == "" || payload.Target.Tag == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "target repository and tag are required",
		})
	}

	// Construct full image name: Registry Host + Repository
	image := payload.Target.Repository
	if payload.Request.Host != "" {
		image = payload.Request.Host + "/" + payload.Target.Repository
	}

	job := gitManager.Job{
		Action:    gitManager.JobActionUpdate,
		Image:     image,
		Tag:       payload.Target.Tag,
		Timestamp: payload.Timestamp,
	}
	if err := gitManager.ValidateUpdateJob(job); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error(), "code": "invalid_update"})
	}

	if payload.ID == "" {
		payload.ID = fmt.Sprintf("generated-%d", time.Now().UnixNano())
	}
	scope := "zot:" + payload.Request.Host
	job.ID = deliveryJobID(scope, payload.ID)
	if job.Timestamp.IsZero() {
		job.Timestamp = time.Now()
	}

	inserted, err := jobStore.EnqueueScoped(job, scope, webhookPayloadHash(c.Body()))
	if err != nil {
		if errors.Is(err, gitManager.ErrIdempotencyConflict) {
			return idempotencyConflict(c)
		}
		log.Printf("Failed to persist Zot job %s: %v", job.ID, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to persist update job",
		})
	}

	if inserted {
		select {
		case jobQueue <- job:
		default:
			// The worker will claim this persisted job after its current work.
		}
	}

	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"status":  "accepted",
		"message": "persisted update job for " + image,
		"jobId":   job.ID,
	})
}

func idempotencyConflict(c *fiber.Ctx) error {
	return c.Status(fiber.StatusConflict).JSON(fiber.Map{
		"error": "job ID was already used for a different request", "code": "idempotency_conflict",
	})
}

// Keep delivery namespaces independent of client-selected API job IDs and of
// other provider connections. Clients must use the returned opaque jobId.
func deliveryJobID(scope, delivery string) string {
	data, _ := json.Marshal([]string{scope, delivery})
	digest := sha256.Sum256(data)
	return "delivery-" + hex.EncodeToString(digest[:])
}

func webhookPayloadHash(body []byte) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "" // Callers have already validated the JSON payload.
	}
	canonical, _ := json.Marshal(value)
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}
