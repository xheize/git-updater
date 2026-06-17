package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

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

// Job represents a queued update task
type Job struct {
	Request      UpdateRequest
	ResponseChan chan JobResponse
}

type JobResponse struct {
	Result UpdateResult
	Err    error
}

type UpdateResult struct {
	NoChanges             bool
	ConflictedAndReverted bool
}

type RepoQueue struct {
	jobs chan Job
}

var (
	repoQueues = make(map[string]*RepoQueue)
	queuesMu   sync.Mutex
)

// getRepoQueue returns or creates a queue and worker for a specific repository
func getRepoQueue(repo string) *RepoQueue {
	queuesMu.Lock()
	defer queuesMu.Unlock()

	q, exists := repoQueues[repo]
	if !exists {
		q = &RepoQueue{
			jobs: make(chan Job, 100),
		}
		repoQueues[repo] = q
		go repoWorker(repo, q)
	}
	return q
}

// repoWorker processes updates for a specific repository sequentially
func repoWorker(repo string, q *RepoQueue) {
	log.Printf("[Worker: %s] Started", repo)
	for job := range q.jobs {
		log.Printf("[Worker: %s] Processing job for branch: %s, file: %s", repo, job.Request.Branch, job.Request.FilePath)

		var res UpdateResult
		var err error

		// Retry loop to handle optimistic concurrency (e.g. network/push blips)
		const maxAttempts = 3
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			res, err = performUpdate(job.Request)
			if err == nil {
				log.Printf("[Worker: %s] Job completed successfully on attempt %d", repo, attempt)
				break
			}
			log.Printf("[Worker: %s] Attempt %d/%d failed: %v", repo, attempt, maxAttempts, err)
		}

		job.ResponseChan <- JobResponse{
			Result: res,
			Err:    err,
		}
	}
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

		q := getRepoQueue(req.Repository)

		responseChan := make(chan JobResponse)
		job := Job{
			Request:      req,
			ResponseChan: responseChan,
		}

		// Enqueue the job
		select {
		case q.jobs <- job:
		default:
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "job queue for repository is full, try again later",
			})
		}

		// Wait for the result from the worker
		res := <-responseChan
		if res.Err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": res.Err.Error(),
			})
		}

		if res.Result.NoChanges {
			return c.JSON(fiber.Map{
				"status":  "no_changes",
				"message": "no changes detected in the YAML file",
			})
		}

		if res.Result.ConflictedAndReverted {
			return c.JSON(fiber.Map{
				"status":  "conflicted_and_reverted",
				"message": "conflict detected with user commits; changes were committed and immediately reverted to preserve user commits",
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

// performUpdate clones, updates, commits and pushes changes for a request
func performUpdate(req UpdateRequest) (UpdateResult, error) {
	var result UpdateResult

	// 1. Create temporary directory
	tmpDir, err := os.MkdirTemp("", "git-updater-")
	if err != nil {
		return result, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// 2. Clone repository (do NOT use depth 1 so we can merge/pull/revert properly)
	gitSSHCmd := "ssh -o StrictHostKeyChecking=no"
	cloneCmd := exec.Command("git", "clone", "-b", req.Branch, req.Repository, tmpDir)
	cloneCmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCmd)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		return result, fmt.Errorf("failed to clone repository: %s: %w", string(out), err)
	}

	// Configure local git identity
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
		return result, fmt.Errorf("failed to read target file: %w", err)
	}

	// 4. Update the specified YAML keys
	updatedBytes, err := processYAMLUpdates(yamlBytes, req.Updates)
	if err != nil {
		return result, fmt.Errorf("failed to update YAML: %w", err)
	}

	// Write updated YAML back to file
	if err := os.WriteFile(fullFilePath, updatedBytes, 0644); err != nil {
		return result, fmt.Errorf("failed to write target file: %w", err)
	}

	// 5. Check git status
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = tmpDir
	statusOut, err := statusCmd.Output()
	if err != nil {
		return result, fmt.Errorf("failed to run git status: %w", err)
	}

	if len(strings.TrimSpace(string(statusOut))) == 0 {
		result.NoChanges = true
		return result, nil
	}

	// 6. Commit locally
	addCmd := exec.Command("git", "add", req.FilePath)
	addCmd.Dir = tmpDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		return result, fmt.Errorf("failed to stage file: %s: %w", string(out), err)
	}

	commitCmd := exec.Command("git", "commit", "-m", "chore: auto-update "+req.FilePath+" via webhook")
	commitCmd.Dir = tmpDir
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return result, fmt.Errorf("failed to commit changes: %s: %w", string(out), err)
	}

	// 7. Pull to check for remote updates and conflicts
	pullCmd := exec.Command("git", "pull", "origin", req.Branch)
	pullCmd.Dir = tmpDir
	pullCmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCmd)
	pullOut, pullErr := pullCmd.CombinedOutput()

	if pullErr != nil {
		log.Printf("[Worker] Pull failed (possible conflict): %s. Reverting update.", string(pullOut))

		// Abort the failed merge/pull
		abortCmd := exec.Command("git", "merge", "--abort")
		abortCmd.Dir = tmpDir
		_ = abortCmd.Run()

		// 8. Revert flow:
		// Reset local branch to the latest remote state
		resetCmd := exec.Command("git", "reset", "--hard", "origin/"+req.Branch)
		resetCmd.Dir = tmpDir
		if out, err := resetCmd.CombinedOutput(); err != nil {
			return result, fmt.Errorf("failed to reset to remote: %s: %w", string(out), err)
		}

		// Re-apply the changes to create the conflicted commit history
		if err := os.WriteFile(fullFilePath, updatedBytes, 0644); err != nil {
			return result, fmt.Errorf("failed to write target file in revert flow: %w", err)
		}

		addConfCmd := exec.Command("git", "add", req.FilePath)
		addConfCmd.Dir = tmpDir
		_ = addConfCmd.Run()

		commitConfCmd := exec.Command("git", "commit", "-m", "chore: auto-update "+req.FilePath+" (conflicted)")
		commitConfCmd.Dir = tmpDir
		if out, err := commitConfCmd.CombinedOutput(); err != nil {
			return result, fmt.Errorf("failed to commit conflicted update: %s: %w", string(out), err)
		}

		// Revert that commit immediately to restore user's original commit state
		revertCmd := exec.Command("git", "revert", "HEAD", "--no-edit")
		revertCmd.Dir = tmpDir
		revertCmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCmd)
		if out, err := revertCmd.CombinedOutput(); err != nil {
			return result, fmt.Errorf("failed to revert conflicted commit: %s: %w", string(out), err)
		}

		// Push both the conflicted commit and the revert commit to remote
		pushCmd := exec.Command("git", "push", "origin", req.Branch)
		pushCmd.Dir = tmpDir
		pushCmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCmd)
		if out, err := pushCmd.CombinedOutput(); err != nil {
			return result, fmt.Errorf("failed to push revert flow: %s: %w", string(out), err)
		}

		result.ConflictedAndReverted = true
		return result, nil
	}

	// 9. Push if pull was clean
	pushCmd := exec.Command("git", "push", "origin", req.Branch)
	pushCmd.Dir = tmpDir
	pushCmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSHCmd)
	if out, err := pushCmd.CombinedOutput(); err != nil {
		return result, fmt.Errorf("failed to push changes: %s: %w", string(out), err)
	}

	return result, nil
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
