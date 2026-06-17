package main

import (
	"log"
	"os"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

func main() {
	// Initialize Fiber app
	app := fiber.New(fiber.Config{
		AppName: "Git Updater v0.1.0",
	})

	// Add middlewares
	app.Use(logger.New())
	app.Use(recover.New())

	// Health Check / Root endpoint
	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":  "healthy",
			"message": "Git Updater service is running",
		})
	})

	// Draft API endpoint for update triggers
	app.Post("/update", func(c *fiber.Ctx) error {
		// This endpoint will trigger git updates in the future.
		// For now, it returns a placeholder response.
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"status":  "accepted",
			"message": "Update trigger received (draft)",
		})
	})

	// Get port from environment or default to 3000
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	log.Printf("Starting server on port %s...", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
