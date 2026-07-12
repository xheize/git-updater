package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/xheize/git-updater/internal/gitManager"
	"github.com/xheize/git-updater/internal/tracker"
)

func main() {
	jobQueue := make(chan gitManager.Job, 100)
	repoURL := os.Getenv("GIT_REPOSITORY_URL")
	if repoURL == "" {
		repoURL = os.Getenv("GIT_REPO_URL")
	}
	if repoURL == "" {
		log.Fatalf("Failed to get git repo url. check env setting")
	}

	// Initialize tracker with default auto-update enabled
	_ = os.MkdirAll("./workspace", 0755)
	t := tracker.New("./workspace/jobs_history.json", true)

	gitMgr := gitManager.New(repoURL, jobQueue, t)
	if gitMgr == nil {
		log.Fatalf("Failed to initialize Git Manager. Check repository and authentication settings.")
	}
	gitMgr.StartWorker(context.Background())

	app := fiber.New(fiber.Config{
		AppName: "Git Updater API",
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	app.Use(logger.New())
	app.Use(recover.New())

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "healthy"})
	})

	// Simple status check endpoint for current history
	app.Get("/api/jobs", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"autoUpdate": t.IsAutoUpdate(),
			"jobs":       t.GetJobs(),
		})
	})

	app.Post("/api/toggle-auto", func(c *fiber.Ctx) error {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		t.SetAutoUpdate(req.Enabled)
		return c.JSON(fiber.Map{
			"status":     "success",
			"autoUpdate": t.IsAutoUpdate(),
		})
	})

	app.Post("/webhook", func(c *fiber.Ctx) error {
		var job gitManager.Job
		if err := c.BodyParser(&job); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "failed to parse request body: " + err.Error(),
			})
		}

		if job.ID == "" {
			job.ID = fmt.Sprintf("webhook-%d", time.Now().UnixNano())
		}
		if job.Timestamp.IsZero() {
			job.Timestamp = time.Now()
		}

		// Enqueue the job
		select {
		case jobQueue <- job:
		default:
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "job queue for repository is full, try again later",
			})
		}

		return c.JSON(fiber.Map{
			"status":  "success",
			"message": "successfully updated " + job.File,
		})
	})

	app.Post("/api/update", func(c *fiber.Ctx) error {
		var job gitManager.Job
		if err := c.BodyParser(&job); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "failed to parse request body: " + err.Error(),
			})
		}

		if job.File == "" || job.Image == "" || job.Tag == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "parameters 'file', 'image', and 'tag' are required",
			})
		}

		if job.ID == "" {
			job.ID = fmt.Sprintf("api-%d", time.Now().UnixNano())
		}
		if job.Timestamp.IsZero() {
			job.Timestamp = time.Now()
		}

		// Enqueue the job
		select {
		case jobQueue <- job:
		default:
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "job queue for repository is full, try again later",
			})
		}

		return c.JSON(fiber.Map{
			"status":  "success",
			"message": "successfully queued update for " + job.File,
			"jobId":   job.ID,
		})
	})

	log.Printf("Server starting on port %s...", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
