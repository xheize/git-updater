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
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Validate secrets before opening the database or contacting Git.
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("invalid server configuration: %w", err)
	}
	store, err := gitManager.NewSQLiteJobStore(cfg.databasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	if recovered, err := store.RecoverInterruptedJobs(); err != nil {
		return err
	} else if recovered > 0 {
		log.Printf("Recovered %d interrupted jobs", recovered)
	}
	queue := make(chan gitManager.Job, 100)
	manager := gitManager.New(cfg.repoURL, queue, store)
	if manager == nil {
		return fmt.Errorf("failed to initialize Git manager; inspect configuration and Git errors above")
	}
	branch, err := manager.BranchName()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerDone := manager.StartWorker(ctx)
	app := fiber.New(fiber.Config{AppName: "Git Updater API"})
	app.Use(logger.New(logger.Config{Next: func(c *fiber.Ctx) bool { return c.Path() == "/health" || c.Path() == "/ready" }}))
	app.Use(recover.New())
	setupHealthRoutes(app, ctx, workerDone, store)
	setupRoutes(app, queue, store, branch, cfg)
	if !cfg.githubEnabled {
		log.Print("GitHub webhook endpoint disabled; configure GITHUB_WEBHOOK_SECRET to enable it")
	}
	listenErrors := make(chan error, 1)
	go func() { listenErrors <- app.Listen(":" + cfg.port) }()
	select {
	case <-ctx.Done():
	case err = <-listenErrors:
		stop()
	}
	// Stop claiming new jobs; leave pending/retrying rows for the next Pod.
	// The current job may finish, with each Git network operation capped at 30s.
	if shutdownErr := app.ShutdownWithTimeout(10 * time.Second); shutdownErr != nil {
		log.Printf("HTTP shutdown: %v", shutdownErr)
	}
	<-workerDone
	return err
}

func setupHealthRoutes(app *fiber.App, ctx context.Context, workerDone <-chan struct{}, store *gitManager.JobStore) {
	app.Get("/health", func(c *fiber.Ctx) error { return c.JSON(fiber.Map{"status": "healthy"}) })
	app.Get("/ready", func(c *fiber.Ctx) error {
		select {
		case <-ctx.Done():
			return c.SendStatus(fiber.StatusServiceUnavailable)
		case <-workerDone:
			return c.SendStatus(fiber.StatusServiceUnavailable)
		default:
		}
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := store.Ping(pingCtx); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		return c.JSON(fiber.Map{"status": "ready"})
	})
}
