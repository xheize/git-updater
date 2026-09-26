package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/xheize/git-updater/internal/gitManager"
)

func validTestConfig(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"API_KEY": "test-api-key", "WEBHOOK_SECRET": "", "GIT_REPOSITORY_URL": "https://example.com/repo.git", "GIT_REPO_URL": "",
		"GIT_AUTH_METHOD": "http", "GIT_USERNAME": "test", "GIT_PASSWORD": "test-password",
		"GITHUB_WEBHOOK_SECRET": "", "GITHUB_WEBHOOK_ENABLED": "", "PORT": "", "JOB_DB_PATH": "",
		"GIT_SSH_PRIVATE_KEY": "", "GIT_SSH_KNOWN_HOSTS_FILE": "",
	} {
		t.Setenv(key, value)
	}
}

func TestConfigRequiresAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		invalid bool
		github  bool
	}{
		{name: "defaults"},
		{name: "missing API key", env: map[string]string{"API_KEY": ""}, invalid: true},
		{name: "legacy API key", env: map[string]string{"API_KEY": "", "WEBHOOK_SECRET": "legacy"}},
		{name: "GitHub enabled without secret", env: map[string]string{"GITHUB_WEBHOOK_ENABLED": "true"}, invalid: true},
		{name: "GitHub secret enables route", env: map[string]string{"GITHUB_WEBHOOK_SECRET": "github"}, github: true},
		{name: "GitHub explicitly disabled", env: map[string]string{"GITHUB_WEBHOOK_SECRET": "github", "GITHUB_WEBHOOK_ENABLED": "false"}},
		{name: "missing repository", env: map[string]string{"GIT_REPOSITORY_URL": ""}, invalid: true},
		{name: "missing SSH known hosts", env: map[string]string{"GIT_AUTH_METHOD": "ssh", "GIT_SSH_PRIVATE_KEY": "test"}, invalid: true},
		{name: "invalid port", env: map[string]string{"PORT": "0"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validTestConfig(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			cfg, err := loadConfig()
			if (err != nil) != tc.invalid {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if !tc.invalid && cfg.githubEnabled != tc.github {
				t.Fatalf("GitHub enabled = %v", cfg.githubEnabled)
			}
		})
	}
}

func TestInvalidConfigDoesNotCreateDatabase(t *testing.T) {
	validTestConfig(t)
	t.Setenv("API_KEY", "")
	path := filepath.Join(t.TempDir(), "data", "jobs.db")
	t.Setenv("JOB_DB_PATH", path)
	if err := run(); err == nil {
		t.Fatal("expected startup to fail")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("database directory created before validation: %v", err)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	for _, state := range []string{"ready", "stopping", "worker-stopped", "database-closed"} {
		t.Run(state, func(t *testing.T) {
			store, err := gitManager.NewSQLiteJobStore(filepath.Join(t.TempDir(), "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			switch state {
			case "stopping":
				cancel()
			case "worker-stopped":
				close(done)
			case "database-closed":
				store.Close()
			}
			app := fiber.New()
			setupHealthRoutes(app, ctx, done, store)
			for _, path := range []string{"/health", "/ready"} {
				response, err := app.Test(httptest.NewRequest("GET", path, nil))
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				want := 200
				if path == "/ready" && state != "ready" {
					want = 503
				}
				if response.StatusCode != want {
					t.Fatalf("%s: got %d want %d", path, response.StatusCode, want)
				}
			}
		})
	}
}

func TestMissingSecretsNeverBypassMiddleware(t *testing.T) {
	for _, middleware := range []fiber.Handler{authMiddleware(""), githubSignatureMiddleware("")} {
		app := fiber.New()
		app.Get("/", middleware, func(c *fiber.Ctx) error { t.Error("unauthenticated handler reached"); return c.SendStatus(200) })
		response, err := app.Test(httptest.NewRequest("GET", "/", nil))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 503 {
			t.Fatalf("got %d", response.StatusCode)
		}
	}
}

func TestOperationalRoutes(t *testing.T) {
	store, err := gitManager.NewSQLiteJobStore(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app := fiber.New()
	setupRoutes(app, make(chan gitManager.Job, 1), store, "main", serverConfig{apiKey: "key"})
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{"POST", "/webhook/github", "", 404},
		{"GET", "/api/status", "", 401},
		{"GET", "/api/status", "key", 200},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		response, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != tc.want {
			t.Fatalf("%s: got %d want %d", tc.path, response.StatusCode, tc.want)
		}
	}
}
