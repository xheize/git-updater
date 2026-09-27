package main

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/xheize/git-updater/internal/controller"
	"github.com/xheize/git-updater/internal/gitManager"
)

type changeController interface {
	Repository(context.Context) (gitManager.RepositoryView, error)
	Preview(context.Context, controller.Intent) (controller.Plan, error)
	ApplyPlan(context.Context, string) (controller.Plan, error)
}

func controllerError(c *fiber.Ctx, err error) error {
	status, code, message := 503, "controller_unavailable", "Controller operation failed; inspect server logs"
	switch {
	case errors.Is(err, controller.ErrNotFound):
		status, code, message = 404, "not_found", err.Error()
	case errors.Is(err, controller.ErrConflict):
		status, code, message = 409, "plan_conflict", err.Error()
	case errors.Is(err, controller.ErrInvalid) || errors.Is(err, controller.ErrNoMatch):
		status, code, message = 422, "invalid_plan", err.Error()
	}
	log.Printf("Controller operation: %v", err)
	return c.Status(status).JSON(fiber.Map{"code": code, "error": message})
}
func pagination(c *fiber.Ctx) (int, int, error) {
	limit, offset := 20, 0
	var err error
	if v := c.Query("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil {
			return 0, 0, err
		}
	}
	if v := c.Query("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil {
			return 0, 0, err
		}
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 1000000 {
		return 0, 0, controller.ErrInvalid
	}
	return limit, offset, nil
}
func setupControllerRoutes(app *fiber.App, engine changeController, store *gitManager.JobStore, key string) {
	api := app.Group("/api", authMiddleware(key))
	api.Get("/repository", func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.UserContext(), 2*time.Minute)
		defer cancel()
		v, err := engine.Repository(ctx)
		if err != nil {
			return controllerError(c, err)
		}
		c.Set("Cache-Control", "no-store")
		return c.JSON(v)
	})
	api.Get("/jobs", func(c *fiber.Ctx) error {
		limit, offset, err := pagination(c)
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid pagination"})
		}
		items, err := store.ListJobs(limit, offset)
		if err != nil {
			return controllerError(c, err)
		}
		return c.JSON(fiber.Map{"items": items, "limit": limit, "offset": offset})
	})
	api.Post("/changesets", func(c *fiber.Ctx) error {
		var in controller.Intent
		if err := c.BodyParser(&in); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid intent JSON"})
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 3*time.Minute)
		defer cancel()
		p, err := engine.Preview(ctx, in)
		if err != nil {
			return controllerError(c, err)
		}
		return c.Status(200).JSON(p)
	})
	api.Get("/changesets", func(c *fiber.Ctx) error {
		limit, offset, err := pagination(c)
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid pagination"})
		}
		items, err := store.ListPlans(limit, offset)
		if err != nil {
			return controllerError(c, err)
		}
		return c.JSON(fiber.Map{"items": items, "limit": limit, "offset": offset})
	})
	api.Get("/changesets/:id", func(c *fiber.Ctx) error {
		p, err := store.GetPlan(c.Params("id"))
		if err != nil {
			return controllerError(c, err)
		}
		return c.JSON(p)
	})
	api.Post("/changesets/:id/apply", func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.UserContext(), 3*time.Minute)
		defer cancel()
		p, err := engine.ApplyPlan(ctx, c.Params("id"))
		if err != nil {
			return controllerError(c, err)
		}
		return c.JSON(p)
	})
}
