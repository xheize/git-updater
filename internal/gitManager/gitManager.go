package gitManager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/go-git/go-git/v6"
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
	repoURL   string
	repo      *git.Repository
	jobQueue  chan Job
	workspace string
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
	fmt.Printf("repoUrl: %s\n", repoUrl)
	fmt.Printf("workspace: %s\n", workspace)

	gitAuthOptions, err := getGitAuth()
	if err != nil {
		return nil
	}

	gitRepo, err := git.PlainClone(workspace, &git.CloneOptions{
		URL:               repoUrl,
		ClientOptions:     gitAuthOptions,
		NoCheckout:        false,
		RecurseSubmodules: git.NoRecurseSubmodules,
	})

	if err != nil {
		log.Println("Clone failed:", err)
		return nil
	}
	fmt.Printf("Repo Clone success!")

	return &gitManager{
		repoURL:   repoUrl,
		repo:      gitRepo,
		jobQueue:  _jobQueue,
		workspace: workspace,
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

func (g *gitManager) StartWorker(ctx context.Context) {
	go func() {
		for {
			select {
			case job := <-g.jobQueue:
				g.Work(job)
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
	data, err := os.ReadFile(job.File)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("File %s does not exist\n", job.File)
		} else {
			fmt.Printf("File %s cannot be read: %v\n", job.File, err)
		}
		return false
	}

	update := map[string]string{
		"spec.template.spec.containers[0].image": job.Image,
	}

	updatedData, err := yaml.ProcessYAMLUpdates(data, update)
	if err != nil {
		fmt.Printf("Update failed for file %s: %v\n", job.File, err)
		return false
	}

	if err := os.WriteFile(job.File, updatedData, 0644); err != nil {
		fmt.Printf("Failed to write file %s: %v\n", job.File, err)
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

	if err := g.repo.Push(&git.PushOptions{}); err != nil {
		fmt.Printf("Git push failed: %v\n", err)
		return false
	}
	return true
}
