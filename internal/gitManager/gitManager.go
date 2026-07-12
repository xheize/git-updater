package gitManager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
	"github.com/xheize/git-updater/internal/tracker"
	"github.com/xheize/git-updater/internal/yaml"
)

const (
	GitAuthTypeSSH  GitAuthType = "ssh"
	GitAuthTypeHTTP GitAuthType = "http"
)

type GitAuthType string

type gitManager struct {
	repoURL   string
	repo      *git.Repository
	jobQueue  chan Job
	workspace string
	tracker   *tracker.Tracker
	authOpts  []client.Option
}

type Job struct {
	ID        string    `json:"id"`
	File      string    `json:"file"`
	Image     string    `json:"image"`
	Tag       string    `json:"tag"`
	Timestamp time.Time `json:"timestamp"`
}

func New(_repoURL string, _jobQueue chan Job, t *tracker.Tracker) *gitManager {
	var repoUrl string

	if _repoURL == "" {
		return nil
	}
	repoUrl = _repoURL
	workspace := "./workspace"
	fmt.Printf("repoUrl: %s\n", repoUrl)
	fmt.Printf("workspace: %s\n", workspace)

	gitAuthOptions, err := getGitAuth()
	if err != nil {
		return nil
	}

	var gitRepo *git.Repository
	gitRepo, err = git.PlainOpen(workspace)
	if err == nil {
		fmt.Printf("Workspace exists. Opened existing repository.\n")
		manager := &gitManager{
			repoURL:   repoUrl,
			repo:      gitRepo,
			jobQueue:  _jobQueue,
			workspace: workspace,
			tracker:   t,
			authOpts:  gitAuthOptions,
		}
		syncErr := manager.syncRepository()
		if syncErr == nil {
			fmt.Printf("Initial repository sync success!\n")
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
	fmt.Printf("Repo Clone success!\n")

	return &gitManager{
		repoURL:   repoUrl,
		repo:      gitRepo,
		jobQueue:  _jobQueue,
		workspace: workspace,
		tracker:   t,
		authOpts:  gitAuthOptions,
	}
}

func getGitAuth() ([]client.Option, error) {
	gitAuthMethod := os.Getenv("GIT_AUTH_METHOD")

	switch gitAuthMethod {
	case string(GitAuthTypeSSH):
		fmt.Printf("gitAuthMethod: %s\n", gitAuthMethod)
		sshPrivateKey := os.Getenv("GIT_SSH_PRIVATE_KEY")
		if sshPrivateKey == "" {
			return nil, errors.New("GIT_SSH_PRIVATE_KEY is empty")
		}

		sign, err := ssh.ParsePrivateKey([]byte(sshPrivateKey))
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
		fmt.Printf("gitAuthMethod: %s\n", gitAuthMethod)
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

func (g *gitManager) StartWorker(ctx context.Context) {
	go func() {
		for {
			select {
			case job := <-g.jobQueue:
				if g.tracker.IsAutoUpdate() {
					g.Work(job)
				} else {
					g.tracker.AddJob(job.ID, job.File, job.Image, job.Tag, tracker.StatusPending)
				}
			case <-ctx.Done():
				log.Printf("waiting for %d jobs to complete", len(g.jobQueue))
				for job := range g.jobQueue {
					g.Work(job)
				}
				log.Printf("Git worker stopped")
				return
			}
		}
	}()
}

func (g *gitManager) Work(job Job) bool {
	g.tracker.AddJob(job.ID, job.File, job.Image, job.Tag, tracker.StatusRunning)

	// Sync workspace with remote branch before reading
	if err := g.syncRepository(); err != nil {
		errMsg := fmt.Sprintf("Failed to sync repository before work: %v", err)
		fmt.Println(errMsg)
		g.tracker.UpdateJobStatus(job.ID, tracker.StatusFailed, errMsg)
		return false
	}

	// Read job file from the workspace path
	filePath := filepath.Join(g.workspace, job.File)
	data, err := os.ReadFile(filePath)
	if err != nil {
		var errMsg string
		if errors.Is(err, os.ErrNotExist) {
			errMsg = fmt.Sprintf("File %s does not exist", filePath)
		} else {
			errMsg = fmt.Sprintf("File %s cannot be read: %v", filePath, err)
		}
		fmt.Println(errMsg)
		g.tracker.UpdateJobStatus(job.ID, tracker.StatusFailed, errMsg)
		return false
	}

	imageWithTag := job.Image
	if job.Tag != "" {
		imageWithTag = job.Image + ":" + job.Tag
	}

	update := map[string]string{
		"spec.template.spec.containers[0].image": imageWithTag,
	}

	updatedData, err := yaml.ProcessYAMLUpdates(data, update)
	if err != nil {
		errMsg := fmt.Sprintf("Update failed for file %s: %v", filePath, err)
		fmt.Println(errMsg)
		g.tracker.UpdateJobStatus(job.ID, tracker.StatusFailed, errMsg)
		return false
	}

	if err := os.WriteFile(filePath, updatedData, 0644); err != nil {
		errMsg := fmt.Sprintf("Failed to write file %s: %v", filePath, err)
		fmt.Println(errMsg)
		g.tracker.UpdateJobStatus(job.ID, tracker.StatusFailed, errMsg)
		return false
	}

	commitMessage := fmt.Sprintf("Update image %s:%s", job.Image, job.Tag)

	if !g.addCommitPush(job.File, commitMessage) {
		errMsg := "Git push failed"
		g.tracker.UpdateJobStatus(job.ID, tracker.StatusFailed, errMsg)
		return false
	}

	g.tracker.UpdateJobStatus(job.ID, tracker.StatusSuccess, "")
	return true
}

func (g *gitManager) addCommitPush(file string, commitMessage string) bool {
	w, err := g.repo.Worktree()
	if err != nil {
		fmt.Printf("Failed to get worktree: %v\n", err)
		return false
	}

	if _, err := w.Add(file); err != nil {
		fmt.Printf("Git add failed for %s: %v\n", file, err)
		return false
	}

	_, err = w.Commit(commitMessage, &git.CommitOptions{})
	if err != nil {
		fmt.Printf("Git commit failed: %v\n", err)
		return false
	}

	if err := g.repo.Push(&git.PushOptions{
		ClientOptions: g.authOpts,
	}); err != nil {
		fmt.Printf("Git push failed: %v\n", err)
		return false
	}
	return true
}
