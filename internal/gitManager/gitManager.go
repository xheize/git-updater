package gitManager

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
	internalYaml "github.com/xheize/git-updater/internal/yaml"
)

const (
	GitAuthTypeSSH  GitAuthType = "ssh"
	GitAuthTypeHTTP GitAuthType = "http"
)

type GitAuthType string

type gitManager struct {
	repoURL      string
	repo         *git.Repository
	jobQueue     chan Job
	workspace    string
	autoUpdate   bool
	authOpts     []client.Option
	imageToFiles map[string][]string
	mu           sync.Mutex
}

type Job struct {
	ID        string    `json:"id"`
	File      string    `json:"file"`
	Image     string    `json:"image"`
	Tag       string    `json:"tag"`
	Timestamp time.Time `json:"timestamp"`
	Force     bool      `json:"force"`
}

func New(_repoURL string, _jobQueue chan Job) *gitManager {
	var repoUrl string

	if _repoURL == "" {
		return nil
	}
	gitAuthMethod := os.Getenv("GIT_AUTH_METHOD")
	repoUrl = normalizeGitURL(_repoURL, gitAuthMethod)
	workspace := "./workspace"
	log.Printf("repoUrl: %s\n", repoUrl)
	log.Printf("workspace: %s\n", workspace)

	autoUpdate := false
	if os.Getenv("AUTO_UPDATE") == "true" {
		autoUpdate = true
	}
	log.Printf("Auto Update set: %v\n", autoUpdate)

	gitAuthOptions, err := getGitAuth()
	if err != nil {
		return nil
	}

	var gitRepo *git.Repository
	gitRepo, err = git.PlainOpen(workspace)
	if err == nil {
		log.Printf("Workspace exists. Opened existing repository.\n")
		manager := &gitManager{
			repoURL:      repoUrl,
			repo:         gitRepo,
			jobQueue:     _jobQueue,
			workspace:    workspace,
			autoUpdate:   autoUpdate,
			authOpts:     gitAuthOptions,
			imageToFiles: make(map[string][]string),
		}
		syncErr := manager.syncRepository()
		if syncErr == nil {
			log.Printf("Initial repository sync success!\n")
			if err := manager.buildImageMapping(); err != nil {
				log.Printf("Failed to build image mapping: %v\n", err)
			}
			return manager
		}
		log.Printf("Initial sync failed: %v. Re-creating workspace.\n", syncErr)
	}

	if removeErr := os.RemoveAll(workspace); removeErr != nil {
		log.Println("Failed to clear workspace directory:", removeErr)
		return nil
	}

	gitRepo, err = git.PlainClone(workspace, &git.CloneOptions{
		URL:               repoUrl,
		ClientOptions:     gitAuthOptions,
		NoCheckout:        false,
		RecurseSubmodules: git.NoRecurseSubmodules,
	})

	if err != nil {
		log.Println("Clone failed:", err)
		return nil
	}
	log.Printf("Repo Clone success!\n")

	manager := &gitManager{
		repoURL:      repoUrl,
		repo:         gitRepo,
		jobQueue:     _jobQueue,
		workspace:    workspace,
		autoUpdate:   autoUpdate,
		authOpts:     gitAuthOptions,
		imageToFiles: make(map[string][]string),
	}
	if err := manager.buildImageMapping(); err != nil {
		log.Printf("Failed to build image mapping: %v\n", err)
	}
	return manager
}

func getGitAuth() ([]client.Option, error) {
	gitAuthMethod := os.Getenv("GIT_AUTH_METHOD")

	switch gitAuthMethod {
	case string(GitAuthTypeSSH):
		log.Printf("gitAuthMethod: %s\n", gitAuthMethod)
		sshPrivateKey := os.Getenv("GIT_SSH_PRIVATE_KEY")
		if sshPrivateKey == "" {
			return nil, errors.New("GIT_SSH_PRIVATE_KEY is empty")
		}

		var keyBytes []byte
		trimmedKey := strings.TrimSpace(sshPrivateKey)
		if !strings.HasPrefix(trimmedKey, "-----BEGIN") {
			// Treat as file path
			var err error
			keyBytes, err = os.ReadFile(trimmedKey)
			if err != nil {
				return nil, fmt.Errorf("failed to read SSH private key file %s: %w", trimmedKey, err)
			}
		} else {
			keyBytes = []byte(sshPrivateKey)
		}

		sign, err := ssh.ParsePrivateKey(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse SSH private key: %w", err)
		}

		authMethod := &gitssh.PublicKeys{
			User:   "git",
			Signer: sign,
		}

		authMethod.HostKeyCallback = ssh.InsecureIgnoreHostKey()

		authOption := client.WithSSHAuth(authMethod)
		return []client.Option{authOption}, nil

	case string(GitAuthTypeHTTP):
		log.Printf("gitAuthMethod: %s\n", gitAuthMethod)
		username := os.Getenv("GIT_USERNAME")
		password := os.Getenv("GIT_PASSWORD")
		if username == "" || password == "" {
			return nil, errors.New("GIT_USERNAME or GIT_PASSWORD is empty")
		}

		authOption := client.WithHTTPAuth(&http.BasicAuth{
			Username: username,
			Password: password,
		})
		return []client.Option{authOption}, nil

	default:
		return nil, fmt.Errorf("invalid GIT_AUTH_METHOD: %s", gitAuthMethod)
	}
}

func (g *gitManager) syncRepository() error {
	w, err := g.repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	// 1. Fetch latest commits from remote
	err = g.repo.Fetch(&git.FetchOptions{
		RemoteName:    "origin",
		ClientOptions: g.authOpts,
		Force:         true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("failed to fetch: %w", err)
	}

	// 2. Get remote tracking branch for current HEAD
	head, err := g.repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD ref: %w", err)
	}

	branchName := head.Name().Short()
	remoteRefName := plumbing.ReferenceName(fmt.Sprintf("refs/remotes/origin/%s", branchName))

	remoteRef, err := g.repo.Reference(remoteRefName, true)
	if err != nil {
		return fmt.Errorf("failed to find remote tracking branch %s: %w", remoteRefName, err)
	}

	// 3. Reset hard to the remote tracking branch commit
	err = w.Reset(&git.ResetOptions{
		Mode:   git.HardReset,
		Commit: remoteRef.Hash(),
	})
	if err != nil {
		return fmt.Errorf("failed to hard reset to remote branch: %w", err)
	}

	return nil
}

func (g *gitManager) StartWorker() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			job, ok := <-g.jobQueue
			if !ok {
				log.Println("Job queue closed. Git worker stopping...")
				return
			}
			if g.autoUpdate || job.Force {
				g.Work(job)
			} else {
				log.Printf("Skipping job %s: autoUpdate is disabled and job is not forced. Logging only.\n", job.ID)
			}
		}
	}()
	return done
}

func (g *gitManager) Work(job Job) bool {
	// Sync workspace with remote branch before reading
	if err := g.syncRepository(); err != nil {
		log.Printf("Failed to sync repository before work: %v\n", err)
		return false
	}

	var filesToUpdate []string
	baseImage := getBaseImageName(job.Image)

	if job.File != "" {
		filesToUpdate = []string{job.File}
	} else {
		g.mu.Lock()
		filesToUpdate = g.imageToFiles[baseImage]
		g.mu.Unlock()
		if len(filesToUpdate) == 0 {
			log.Printf("Job %s skipped: no files found in repository referencing image %s\n", job.ID, baseImage)
			return true
		}
	}

	var updatedFiles []string
	for _, relPath := range filesToUpdate {
		filePath := filepath.Join(g.workspace, relPath)
		data, err := os.ReadFile(filePath)
		if err != nil {
			log.Printf("File %s cannot be read: %v\n", filePath, err)
			continue
		}

		updatedData, updated, err := internalYaml.ProcessYAMLImageUpdate(data, baseImage, job.Tag)
		if err != nil {
			log.Printf("Update failed for file %s: %v\n", filePath, err)
			continue
		}

		if updated {
			if err := os.WriteFile(filePath, updatedData, 0644); err != nil {
				log.Printf("Failed to write file %s: %v\n", filePath, err)
				continue
			}
			updatedFiles = append(updatedFiles, relPath)
		}
	}

	if len(updatedFiles) == 0 {
		log.Printf("Job %s completed: no files were actually modified.\n", job.ID)
		return true
	}

	commitMessage := fmt.Sprintf("Update image %s:%s in %d files", baseImage, job.Tag, len(updatedFiles))
	if job.File != "" {
		commitMessage = fmt.Sprintf("Update image %s:%s in %s", baseImage, job.Tag, job.File)
	}

	if !g.addCommitPush(updatedFiles, commitMessage) {
		return false
	}

	// Rebuild in-memory mapping after push
	if err := g.buildImageMapping(); err != nil {
		log.Printf("Failed to rebuild image mapping after update: %v\n", err)
	}

	return true
}

func (g *gitManager) addCommitPush(files []string, commitMessage string) bool {
	w, err := g.repo.Worktree()
	if err != nil {
		log.Printf("Failed to get worktree: %v\n", err)
		return false
	}

	for _, file := range files {
		if _, err := w.Add(file); err != nil {
			log.Printf("Git add failed for %s: %v\n", file, err)
			return false
		}
	}

	_, err = w.Commit(commitMessage, &git.CommitOptions{})
	if err != nil {
		log.Printf("Git commit failed: %v\n", err)
		return false
	}

	if err := g.repo.Push(&git.PushOptions{
		ClientOptions: g.authOpts,
	}); err != nil {
		log.Printf("Git push failed: %v\n", err)
		return false
	}
	return true
}

func (g *gitManager) buildImageMapping() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	newMap := make(map[string][]string)

	err := filepath.Walk(g.workspace, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".yaml" || ext == ".yml" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil // Skip unreadable files
			}

			dec := yaml.NewDecoder(bytes.NewReader(data))
			for {
				var doc yaml.Node
				err := dec.Decode(&doc)
				if err != nil {
					break // EOF or parse error
				}

				// Check if it's an ArgoCD Application
				if internalYaml.IsArgoCDApplication(&doc) {
					log.Printf("Skipping ArgoCD Application file: %s\n", path)
					continue
				}

				// Extract container images
				images := extractImagesFromNode(&doc)
				relPath, err := filepath.Rel(g.workspace, path)
				if err == nil {
					for _, img := range images {
						baseImg := getBaseImageName(img)
						if baseImg != "" {
							newMap[baseImg] = appendUnique(newMap[baseImg], relPath)
						}
					}
				}
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	g.imageToFiles = newMap
	log.Printf("Built in-memory image mapping: %d unique images mapped\n", len(g.imageToFiles))
	for img, files := range g.imageToFiles {
		log.Printf("  Image: %s -> Files: %v\n", img, files)
	}
	return nil
}

func extractImagesFromNode(node *yaml.Node) []string {
	var images []string
	if node.Kind == yaml.DocumentNode {
		for _, child := range node.Content {
			images = append(images, extractImagesFromNode(child)...)
		}
		return images
	}

	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i].Value
			valNode := node.Content[i+1]
			if key == "image" && valNode.Kind == yaml.ScalarNode {
				images = append(images, valNode.Value)
			} else {
				images = append(images, extractImagesFromNode(valNode)...)
			}
		}
	}

	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			images = append(images, extractImagesFromNode(child)...)
		}
	}

	return images
}

func getBaseImageName(imageStr string) string {
	idx := strings.LastIndex(imageStr, ":")
	if idx == -1 {
		return imageStr
	}
	suffix := imageStr[idx+1:]
	if strings.Contains(suffix, "/") {
		return imageStr
	}
	return imageStr[:idx]
}

func appendUnique(slice []string, val string) []string {
	for _, item := range slice {
		if item == val {
			return slice
		}
	}
	return append(slice, val)
}

func normalizeGitURL(rawURL string, authMethod string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}

	isHTTPS := strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://")
	isSSH := strings.HasPrefix(rawURL, "git@") || strings.HasPrefix(rawURL, "ssh://")

	if authMethod == "ssh" && isHTTPS {
		// Convert HTTPS -> SSH (git@host:path)
		trimmed := rawURL
		if strings.HasPrefix(trimmed, "https://") {
			trimmed = strings.TrimPrefix(trimmed, "https://")
		} else {
			trimmed = strings.TrimPrefix(trimmed, "http://")
		}

		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) == 2 {
			host := parts[0]
			path := parts[1]
			return fmt.Sprintf("git@%s:%s", host, path)
		}
	}

	if authMethod == "http" && isSSH {
		// Convert SSH -> HTTPS
		if strings.HasPrefix(rawURL, "ssh://git@") {
			trimmed := strings.TrimPrefix(rawURL, "ssh://git@")
			return "https://" + trimmed
		}
		if strings.HasPrefix(rawURL, "git@") {
			trimmed := strings.TrimPrefix(rawURL, "git@")
			trimmed = strings.Replace(trimmed, ":", "/", 1)
			return "https://" + trimmed
		}
	}

	return rawURL
}
