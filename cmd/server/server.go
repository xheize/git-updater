package main

import (
	"context"
	"log"
	"os"

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

	app.Post("/webhook", func(c *fiber.Ctx) error {
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

	log.Printf("Server starting on port %s...", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
