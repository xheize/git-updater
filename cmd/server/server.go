package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/xheize/git-updater/internal/gitManager"
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

	gitMgr := gitManager.New(repoURL, jobQueue)
	if gitMgr == nil {
		log.Fatalf("Failed to initialize Git Manager. Check repository and authentication settings.")
	}
	workerDone := gitMgr.StartWorker()

	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("WEBHOOK_SECRET")
	}
	if apiKey == "" {
		log.Println("WARNING: API_KEY environment variable is not set. API endpoints will not be authenticated.")
	}

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

	auth := authMiddleware(apiKey)

	app.Post("/webhook", auth, func(c *fiber.Ctx) error {
		var job gitManager.Job
		if err := c.BodyParser(&job); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "failed to parse request body: " + err.Error(),
			})
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

	app.Post("/api/update", auth, func(c *fiber.Ctx) error {
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

	// Create context that listens for the interrupt signals
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Server starting on port %s...", port)
		if err := app.Listen(":" + port); err != nil {
			log.Printf("Server failed to start: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Gracefully shutting down...")

	// 1. Shutdown Fiber web server first (stop accepting new requests)
	if err := app.Shutdown(); err != nil {
		log.Printf("Fiber shutdown error: %v", err)
	}

	// 2. Close the job queue so the worker knows no more jobs will be submitted
	close(jobQueue)

	// 3. Wait for the worker to finish processing remaining jobs
	log.Println("Waiting for worker to finish processing remaining jobs...")
	<-workerDone
	log.Println("Server shutdown complete.")
}

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
