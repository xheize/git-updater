package gitManager

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
	"github.com/xheize/git-updater/internal/yaml"
)

const (
	GitAuthTypeSSH  GitAuthType = "ssh"
	GitAuthTypeHTTP GitAuthType = "http"
)

type GitAuthType string

type gitManager struct {
	repoURL    string
	repo       *git.Repository
	jobQueue   chan Job
	workspace  string
	autoUpdate bool
	authOpts   []client.Option
}

type Job struct {
	ID        string    `json:"id"`
	File      string    `json:"file"`
	Image     string    `json:"image"`
	Tag       string    `json:"tag"`
	Timestamp time.Time `json:"timestamp"`
}

func New(_repoURL string, _jobQueue chan Job) *gitManager {
	var repoUrl string

	if _repoURL == "" {
		return nil
	}
	repoUrl = _repoURL
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
			repoURL:    repoUrl,
			repo:       gitRepo,
			jobQueue:   _jobQueue,
			workspace:  workspace,
			autoUpdate: autoUpdate,
			authOpts:   gitAuthOptions,
		}
		syncErr := manager.syncRepository()
		if syncErr == nil {
			log.Printf("Initial repository sync success!\n")
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

	return &gitManager{
		repoURL:    repoUrl,
		repo:       gitRepo,
		jobQueue:   _jobQueue,
		workspace:  workspace,
		autoUpdate: autoUpdate,
		authOpts:   gitAuthOptions,
	}
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
			if g.autoUpdate {
				g.Work(job)
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

	// Read job file from the workspace path
	filePath := filepath.Join(g.workspace, job.File)
	data, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("File %s does not exist\n", filePath)
		} else {
			log.Printf("File %s cannot be read: %v\n", filePath, err)
		}
		return false
	}

	update := map[string]string{
		"spec.template.spec.containers[0].image": job.Image,
	}

	updatedData, err := yaml.ProcessYAMLUpdates(data, update)
	if err != nil {
		log.Printf("Update failed for file %s: %v\n", filePath, err)
		return false
	}

	if err := os.WriteFile(filePath, updatedData, 0644); err != nil {
		log.Printf("Failed to write file %s: %v\n", filePath, err)
		return false
	}

	commitMessage := fmt.Sprintf("Update image %s:%s", job.Image, job.Tag)

	if !g.addCommitPush(job.File, commitMessage) {
		return false
	}

	return true
}

func (g *gitManager) addCommitPush(file string, commitMessage string) bool {
	w, err := g.repo.Worktree()
	if err != nil {
		log.Printf("Failed to get worktree: %v\n", err)
		return false
	}

	if _, err := w.Add(file); err != nil {
		log.Printf("Git add failed for %s: %v\n", file, err)
		return false
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
