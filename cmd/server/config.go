package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type serverConfig struct {
	repoURL, apiKey, githubSecret, databasePath, port string
	githubEnabled                                     bool
}

func loadConfig() (serverConfig, error) {
	cfg := serverConfig{
		repoURL: strings.TrimSpace(os.Getenv("GIT_REPOSITORY_URL")),
		apiKey:  os.Getenv("API_KEY"), githubSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
		databasePath: os.Getenv("JOB_DB_PATH"), port: os.Getenv("PORT"),
	}
	if cfg.repoURL == "" {
		cfg.repoURL = strings.TrimSpace(os.Getenv("GIT_REPO_URL"))
	}
	if cfg.apiKey == "" {
		cfg.apiKey = os.Getenv("WEBHOOK_SECRET")
	}
	if strings.TrimSpace(cfg.apiKey) == "" {
		return cfg, fmt.Errorf("API_KEY (or WEBHOOK_SECRET) is required")
	}
	if cfg.repoURL == "" {
		return cfg, fmt.Errorf("GIT_REPOSITORY_URL is required")
	}
	cfg.githubEnabled = strings.TrimSpace(cfg.githubSecret) != ""
	if enabled := os.Getenv("GITHUB_WEBHOOK_ENABLED"); enabled != "" {
		var err error
		cfg.githubEnabled, err = strconv.ParseBool(enabled)
		if err != nil {
			return cfg, fmt.Errorf("GITHUB_WEBHOOK_ENABLED must be a boolean")
		}
	}
	if cfg.githubEnabled && strings.TrimSpace(cfg.githubSecret) == "" {
		return cfg, fmt.Errorf("GITHUB_WEBHOOK_SECRET is required when GitHub webhooks are enabled")
	}
	switch os.Getenv("GIT_AUTH_METHOD") {
	case "ssh":
		for _, name := range []string{"GIT_SSH_PRIVATE_KEY", "GIT_SSH_KNOWN_HOSTS_FILE"} {
			if strings.TrimSpace(os.Getenv(name)) == "" {
				return cfg, fmt.Errorf("%s is required for SSH", name)
			}
		}
	case "http":
		if os.Getenv("GIT_USERNAME") == "" || os.Getenv("GIT_PASSWORD") == "" {
			return cfg, fmt.Errorf("GIT_USERNAME and GIT_PASSWORD are required for HTTP")
		}
	default:
		return cfg, fmt.Errorf("GIT_AUTH_METHOD must be ssh or http")
	}
	if cfg.databasePath == "" {
		cfg.databasePath = "./data/jobs.db"
	}
	if cfg.port == "" {
		cfg.port = "3000"
	}
	port, err := strconv.Atoi(cfg.port)
	if err != nil || port < 1 || port > 65535 {
		return cfg, fmt.Errorf("PORT must be between 1 and 65535")
	}
	return cfg, nil
}
