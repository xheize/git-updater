package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"github.com/xheize/git-updater/internal/gitManager"
)

func main() {

	jobQueue := make(chan gitManager.Job, 100)
	jobDatabasePath := os.Getenv("JOB_DB_PATH")
	if jobDatabasePath == "" {
		jobDatabasePath = "./data/jobs.db"
	}
	jobStore, err := gitManager.NewSQLiteJobStore(jobDatabasePath)
	if err != nil {
		log.Fatalf("Failed to initialize job store: %v", err)
	}
	defer jobStore.Close()
	if recovered, err := jobStore.RecoverInterruptedJobs(); err != nil {
		log.Fatalf("Failed to recover interrupted jobs: %v", err)
	} else if recovered > 0 {
		log.Printf("Recovered %d interrupted jobs", recovered)
	}

	repoURL := os.Getenv("GIT_REPOSITORY_URL")
	if repoURL == "" {
		repoURL = os.Getenv("GIT_REPO_URL")
	}
	if repoURL == "" {
		log.Fatalf("Failed to get git repo url. check env setting")
	}

	gitMgr := gitManager.New(repoURL, jobQueue, jobStore)
	if gitMgr == nil {
		log.Fatalf("Failed to initialize Git Manager. Check repository and authentication settings.")
	}
	targetBranch, err := gitMgr.BranchName()
	if err != nil {
		log.Fatalf("Failed to determine repository branch: %v", err)
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

	app.Use(logger.New(logger.Config{
		Next: func(c *fiber.Ctx) bool {
			return c.Path() == "/health"
		},
	}))
	app.Use(recover.New())

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "healthy"})
	})

	setupRoutes(app, jobQueue, jobStore, targetBranch)

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
