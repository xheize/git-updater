package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"gopkg.in/yaml.v3"
)

// UpdateRequest represents the payload for the webhook
type UpdateRequest struct {
	Repository string            `json:"repository"` // e.g., git@github.com:xheize/k3s-ops.git
	Branch     string            `json:"branch"`     // e.g., main
	FilePath   string            `json:"file_path"`  // e.g., namespace/zot/statefulset/zot.yaml
	Updates    map[string]string `json:"updates"`    // e.g., {"spec.template.spec.containers[0].image": "ghcr.io/project-zot/zot:v2.1.14"}
}

type PathPart struct {
	Key   string
	Index int // -1 if it's not a slice index
}

func main() {
	app := fiber.New(fiber.Config{
		AppName: "Git Updater API",
	})

	app.Use(logger.New())
	app.Use(recover.New())

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "healthy"})
	})

	app.Post("/webhook", func(c *fiber.Ctx) error {
		var req UpdateRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "failed to parse request body: " + err.Error(),
			})
		}

		if req.Repository == "" || req.FilePath == "" || len(req.Updates) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "repository, file_path, and updates are required fields",
			})
		}

		if req.Branch == "" {
			req.Branch = "main"
		}

		log.Printf("Received update request for repo: %s, branch: %s, file: %s", req.Repository, req.Branch, req.FilePath)

		// 1. Create temporary directory
		tmpDir, err := os.MkdirTemp("", "git-updater-")
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to create temp dir: " + err.Error(),
			})
		}
		defer os.RemoveAll(tmpDir)

		// 2. Clone repository
		// Disable StrictHostKeyChecking for github.com dynamically
		gitSSHCmd := "ssh -o StrictHostKeyChecking=no"
		cloneCmd := exec.Command("git", "clone", "-b", req.Branch, "--depth", "1", req.Repository, tmpDir)
		cloneCmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCmd)
		if out, err := cloneCmd.CombinedOutput(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to clone repository: " + err.Error(),
				"logs":  string(out),
			})
		}

		// Configure local git user for committing
		configUserCmd := exec.Command("git", "config", "user.name", "git-updater")
		configUserCmd.Dir = tmpDir
		_ = configUserCmd.Run()

		configEmailCmd := exec.Command("git", "config", "user.email", "git-updater@lxc.local")
		configEmailCmd.Dir = tmpDir
		_ = configEmailCmd.Run()

		// 3. Read and parse YAML file
		fullFilePath := filepath.Join(tmpDir, req.FilePath)
		yamlBytes, err := os.ReadFile(fullFilePath)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to read target file: " + err.Error(),
			})
		}

		// 4. Update the specified YAML keys
		updatedBytes, err := processYAMLUpdates(yamlBytes, req.Updates)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to update YAML: " + err.Error(),
			})
		}

		// Write updated YAML back to file
		if err := os.WriteFile(fullFilePath, updatedBytes, 0644); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to write target file: " + err.Error(),
			})
		}

		// 5. Check git status
		statusCmd := exec.Command("git", "status", "--porcelain")
		statusCmd.Dir = tmpDir
		statusOut, err := statusCmd.Output()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to run git status: " + err.Error(),
			})
		}

		if len(strings.TrimSpace(string(statusOut))) == 0 {
			return c.JSON(fiber.Map{
				"status":  "no_changes",
				"message": "no changes detected in the YAML file",
			})
		}

		// 6. Commit and Push
		addCmd := exec.Command("git", "add", req.FilePath)
		addCmd.Dir = tmpDir
		if out, err := addCmd.CombinedOutput(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to stage file: " + err.Error(),
				"logs":  string(out),
			})
		}

		commitCmd := exec.Command("git", "commit", "-m", "chore: auto-update "+req.FilePath+" via webhook")
		commitCmd.Dir = tmpDir
		if out, err := commitCmd.CombinedOutput(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to commit changes: " + err.Error(),
				"logs":  string(out),
			})
		}

		pushCmd := exec.Command("git", "push", "origin", req.Branch)
		pushCmd.Dir = tmpDir
		pushCmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCmd)
		if out, err := pushCmd.CombinedOutput(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "failed to push changes: " + err.Error(),
				"logs":  string(out),
			})
		}

		return c.JSON(fiber.Map{
			"status":  "success",
			"message": "successfully updated " + req.FilePath + " and pushed to " + req.Branch,
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	log.Printf("Server starting on port %s...", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

// processYAMLUpdates processes multiple updates on a multi-document YAML content
func processYAMLUpdates(yamlData []byte, updates map[string]string) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(yamlData))
	var documents []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		documents = append(documents, &doc)
	}

	for pathStr, newValue := range updates {
		parts := parsePath(pathStr)
		updated := false
		for _, doc := range documents {
			if updateNode(doc, parts, newValue) {
				updated = true
			}
		}
		if !updated {
			return nil, errors.New("key path not found: " + pathStr)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range documents {
		if err := enc.Encode(doc); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// parsePath parses a dot-notation key path like "spec.template.spec.containers[0].image"
func parsePath(pathStr string) []PathPart {
	parts := strings.Split(pathStr, ".")
	var result []PathPart
	for _, p := range parts {
		if idx := strings.Index(p, "["); idx != -1 && strings.HasSuffix(p, "]") {
			key := p[:idx]
			indexStr := p[idx+1 : len(p)-1]
			index, err := strconv.Atoi(indexStr)
			if err == nil {
				// Add the parent mapping key first
				if key != "" {
					result = append(result, PathPart{Key: key, Index: -1})
				}
				// Add the sequence index part
				result = append(result, PathPart{Key: "", Index: index})
				continue
			}
		}
		result = append(result, PathPart{Key: p, Index: -1})
	}
	return result
}

// updateNode recursively traverses yaml.Node to find the target path and updates its value
func updateNode(node *yaml.Node, parts []PathPart, newValue string) bool {
	if len(parts) == 0 {
		if node.Kind == yaml.ScalarNode {
			node.Value = newValue
			return true
		}
		return false
	}

	part := parts[0]
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if updateNode(child, parts, newValue) {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]
			if keyNode.Value == part.Key {
				return updateNode(valueNode, parts[1:], newValue)
			}
		}
	case yaml.SequenceNode:
		if part.Index >= 0 && part.Index < len(node.Content) {
			return updateNode(node.Content[part.Index], parts[1:], newValue)
		}
	}
	return false
}
