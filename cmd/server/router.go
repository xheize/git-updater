package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/xheize/git-updater/internal/gitManager"
)

// setupRoutes registers all route handlers grouped by API and Webhooks
func setupRoutes(app *fiber.App, jobQueue chan gitManager.Job) {
	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("WEBHOOK_SECRET")
	}

	githubSecret := os.Getenv("GITHUB_WEBHOOK_SECRET")

	// Auth Middleware for standard API and Webhook endpoints
	auth := authMiddleware(apiKey)

	// Webhook Group
	webhooks := app.Group("/webhook")

	// Standard webhook (API key authenticated, used by CLI)
	webhooks.Post("", auth, func(c *fiber.Ctx) error {
		return handleJobEnqueue(c, jobQueue, "webhook")
	})

	// GitHub webhook (GitHub signature verified, GITHUB_WEBHOOK_SECRET key check)
	githubAuth := githubSignatureMiddleware(githubSecret)
	webhooks.Post("/github", githubAuth, func(c *fiber.Ctx) error {
		return handleJobEnqueue(c, jobQueue, "github-webhook")
	})

	// Zot webhook (API key authenticated, parses Zot events extension JSON format)
	webhooks.Post("/zot", auth, func(c *fiber.Ctx) error {
		return handleZotWebhook(c, jobQueue)
	})

	// API Group
	api := app.Group("/api")
	api.Post("/update", auth, func(c *fiber.Ctx) error {
		return handleJobEnqueue(c, jobQueue, "api")
	})
}

// handleJobEnqueue validates the request body, sets default ID/Timestamp, and enqueues the job
func handleJobEnqueue(c *fiber.Ctx, jobQueue chan gitManager.Job, source string) error {
	var job gitManager.Job
	if err := c.BodyParser(&job); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "failed to parse request body: " + err.Error(),
		})
	}

	if job.Image == "" || job.Tag == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "parameters 'image' and 'tag' are required",
		})
	}

	if job.ID == "" {
		job.ID = fmt.Sprintf("%s-%d", source, time.Now().UnixNano())
	}
	if job.Timestamp.IsZero() {
		job.Timestamp = time.Now()
	}

	select {
	case jobQueue <- job:
	default:
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "job queue for repository is full, try again later",
		})
	}

	targetMsg := job.File
	if targetMsg == "" {
		targetMsg = job.Image
	}

	return c.JSON(fiber.Map{
		"status":  "success",
		"message": "successfully queued update for " + targetMsg,
		"jobId":   job.ID,
	})
}

// authMiddleware validates custom API token or Bearer token header
func authMiddleware(apiKey string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if apiKey == "" {
			return c.Next()
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
			log.Println("WARNING: GITHUB_WEBHOOK_SECRET is not set. Skipping signature validation.")
			return c.Next()
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
func handleZotWebhook(c *fiber.Ctx, jobQueue chan gitManager.Job) error {
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
		ID:        payload.ID,
		Image:     image,
		Tag:       payload.Target.Tag,
		Timestamp: payload.Timestamp,
	}

	if job.ID == "" {
		job.ID = fmt.Sprintf("zot-%d", time.Now().UnixNano())
	}
	if job.Timestamp.IsZero() {
		job.Timestamp = time.Now()
	}

	select {
	case jobQueue <- job:
	default:
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "job queue for repository is full, try again later",
		})
	}

	return c.JSON(fiber.Map{
		"status":  "success",
		"message": "successfully queued update for " + image,
		"jobId":   job.ID,
	})
}
