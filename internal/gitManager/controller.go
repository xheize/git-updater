package gitManager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/xheize/git-updater/internal/controller"
)

type RepositoryView struct {
	Model    controller.Model    `json:"model"`
	Identity controller.Identity `json:"identity"`
	Branch   string              `json:"branch"`
	Scope    string              `json:"scope"`
}

func repositoryScope() (string, error) {
	s := strings.TrimSpace(os.Getenv("GITOPS_PATH"))
	if s == "" {
		s = "."
	}
	if strings.ContainsAny(s, "\\:\x00") || path.IsAbs(s) || s == ".." || strings.HasPrefix(path.Clean(s), "../") {
		return "", fmt.Errorf("GITOPS_PATH must remain inside the repository")
	}
	return path.Clean(s), nil
}

func (g *gitManager) snapshot() (controller.Model, map[string]string, error) {
	head, err := g.repo.Head()
	if err != nil {
		return controller.Model{}, nil, err
	}
	commit, err := g.repo.CommitObject(head.Hash())
	if err != nil {
		return controller.Model{}, nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return controller.Model{}, nil, err
	}
	scope, err := repositoryScope()
	if err != nil {
		return controller.Model{}, nil, err
	}
	if scope != "." {
		tree, err = tree.Tree(scope)
		if err != nil {
			return controller.Model{}, nil, fmt.Errorf("GITOPS_PATH does not exist at this revision")
		}
	}
	var checkTree func(*object.Tree) error
	checkTree = func(t *object.Tree) error {
		for _, entry := range t.Entries {
			if entry.Mode == filemode.Submodule {
				return fmt.Errorf("Git submodules are not supported in the selected scope")
			}
			if entry.Mode == filemode.Dir {
				child, err := g.repo.TreeObject(entry.Hash)
				if err != nil {
					return err
				}
				if err := checkTree(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := checkTree(tree); err != nil {
		return controller.Model{}, nil, err
	}
	files := map[string]string{}
	total := int64(0)
	err = tree.Files().ForEach(func(f *object.File) error {
		ext := strings.ToLower(path.Ext(f.Name))
		base := path.Base(f.Name)
		if ext != ".yaml" && ext != ".yml" && base != "Kustomization" {
			return nil
		}
		if f.Mode != filemode.Regular && f.Mode != filemode.Executable {
			return fmt.Errorf("unsupported file mode: %s", f.Name)
		}
		total += f.Size
		if f.Size > 2<<20 || total > 32<<20 || len(files) >= 10000 {
			return fmt.Errorf("repository exceeds MVP parser limits")
		}
		text, err := f.Contents()
		if err != nil {
			return err
		}
		name := f.Name
		if scope != "." {
			name = path.Join(scope, name)
		}
		files[name] = text
		return nil
	})
	if err != nil {
		return controller.Model{}, nil, err
	}
	return controller.Parse(head.Hash().String(), files), files, nil
}
func (g *gitManager) Repository(ctx context.Context) (RepositoryView, error) {
	g.operationMu.Lock()
	defer g.operationMu.Unlock()
	if _, err := g.originURL(); err != nil {
		return RepositoryView{}, err
	}
	if err := g.syncRepository(); err != nil {
		return RepositoryView{}, err
	}
	branch, err := g.BranchName()
	if err != nil {
		return RepositoryView{}, err
	}
	model, _, err := g.snapshot()
	if err != nil {
		return RepositoryView{}, err
	}
	identity, verificationErr := g.verifyRepository(ctx, branch)
	if verificationErr != nil {
		identity.Detail = verificationErr.Error()
	}
	scope, _ := repositoryScope()
	return RepositoryView{model, identity, branch, scope}, nil
}
func (g *gitManager) Preview(ctx context.Context, in controller.Intent) (controller.Plan, error) {
	g.operationMu.Lock()
	defer g.operationMu.Unlock()
	return g.preview(ctx, in)
}
func (g *gitManager) preview(ctx context.Context, in controller.Intent) (controller.Plan, error) {
	var empty controller.Plan
	if err := controller.ValidateIntent(in); err != nil {
		return empty, err
	}
	if existing, err := g.jobStore.GetPlan(in.ID); err == nil {
		if !controller.SameIntent(existing.Intent, in) {
			return empty, fmt.Errorf("%w: intent ID reused with different content", controller.ErrConflict)
		}
		return existing, nil
	} else if !errors.Is(err, controller.ErrNotFound) {
		return empty, err
	}
	if _, err := g.originURL(); err != nil {
		return empty, err
	}
	if err := g.syncRepository(); err != nil {
		return empty, err
	}
	branch, err := g.BranchName()
	if err != nil {
		return empty, err
	}
	if os.Getenv("CONTROLLER_LOCAL_MODE") != "true" {
		if err := g.ensureGitAccess(ctx, branch); err != nil {
			return empty, err
		}
	}
	identity, err := g.verifyRepository(ctx, branch)
	if err != nil {
		return empty, fmt.Errorf("%w: %v", controller.ErrInvalid, err)
	}
	if !identity.Verified || !identity.Writable {
		return empty, fmt.Errorf("%w: repository is not verified/writable or branch is protected", controller.ErrInvalid)
	}
	if err := g.jobStore.bindIdentity(identity.Provider + ":" + identity.RepositoryID + ":" + branch); err != nil {
		return empty, err
	}
	model, files, err := g.snapshot()
	if err != nil {
		return empty, err
	}
	p, err := controller.BuildPlan(model, files, in)
	if err != nil {
		return empty, err
	}
	p.Identity = identity
	p.Branch = branch
	p.Scope, _ = repositoryScope()
	for i, a := range p.Artifacts {
		artifact, err := g.verifyArtifact(ctx, controller.Change{Image: a.Image, Tag: a.Tag})
		if err != nil {
			return empty, fmt.Errorf("%w: %v", controller.ErrInvalid, err)
		}
		p.Artifacts[i] = artifact
	}
	if err := g.jobStore.savePlan(p, true); err != nil {
		return empty, err
	}
	return p, nil
}
func (g *gitManager) ApplyPlan(ctx context.Context, id string) (controller.Plan, error) {
	g.operationMu.Lock()
	defer g.operationMu.Unlock()
	return g.applyPlan(ctx, id)
}
func (g *gitManager) applyPlan(ctx context.Context, id string) (controller.Plan, error) {
	p, err := g.jobStore.GetPlan(id)
	if err != nil {
		return p, err
	}
	if p.State == "published" || p.State == "already_satisfied" {
		return p, nil
	}
	if _, err := g.originURL(); err != nil {
		return p, err
	}
	if err := g.syncRepository(); err != nil {
		return p, err
	}
	branch, err := g.BranchName()
	if err != nil {
		return p, err
	}
	scope, err := repositoryScope()
	if err != nil {
		return p, err
	}
	if p.Scope != scope {
		return p, fmt.Errorf("%w: repository scope changed", controller.ErrConflict)
	}
	if os.Getenv("CONTROLLER_LOCAL_MODE") != "true" {
		if err := g.ensureGitAccess(ctx, branch); err != nil {
			return p, err
		}
	}
	identity, err := g.verifyRepository(ctx, branch)
	if err != nil {
		return p, fmt.Errorf("%w: %v", controller.ErrInvalid, err)
	}
	if !identity.Verified || !identity.Writable || identity.Provider != p.Identity.Provider || identity.RepositoryID != p.Identity.RepositoryID || branch != p.Branch {
		return p, fmt.Errorf("%w: repository identity/access/ref changed", controller.ErrConflict)
	}
	if err := g.jobStore.bindIdentity(identity.Provider + ":" + identity.RepositoryID + ":" + branch); err != nil {
		return p, err
	}
	if p.State == "publishing" || p.State == "unknown" {
		// A push response or database write may have been lost. Never generate a
		// second commit or rebase the old intent; only recover a proven publication.
		if p.Commit != "" && g.reachable(p.Commit) {
			p.State = "published"
			p.Error = ""
			return p, g.jobStore.savePlan(p, false)
		}
		return p, fmt.Errorf("%w: publication outcome unknown; inspect remote history", controller.ErrConflict)
	}
	if p.State != "planned" {
		return p, controller.ErrConflict
	}
	head, err := g.repo.Head()
	if err != nil {
		return p, err
	}
	if head.Hash().String() != p.BaseRevision {
		p.State = "conflict"
		p.Error = "Remote HEAD changed; create and review a new intent"
		if err := g.jobStore.savePlan(p, false); err != nil {
			return p, err
		}
		return p, controller.ErrConflict
	}
	// Recheck artifact identity so moving tags invalidate preview approval.
	for i, artifact := range p.Artifacts {
		a, err := g.verifyArtifact(ctx, controller.Change{Image: artifact.Image, Tag: artifact.Tag})
		if err != nil {
			return p, err
		}
		if i >= len(p.Artifacts) || a != p.Artifacts[i] {
			return p, fmt.Errorf("%w: registry artifact changed", controller.ErrConflict)
		}
	}
	model, files, err := g.snapshot()
	if err != nil {
		return p, err
	}
	expected, err := controller.BuildPlan(model, files, p.Intent)
	if err != nil {
		return p, err
	}
	if !sameMutations(p, expected) {
		return p, fmt.Errorf("%w: plan no longer resolves identically", controller.ErrConflict)
	}
	if len(p.Mutations) == 0 {
		p.State = "already_satisfied"
		return p, g.jobStore.savePlan(p, false)
	}
	updated, err := controller.Apply(files, p.Mutations)
	if err != nil {
		return p, err
	}
	wt, err := g.repo.Worktree()
	if err != nil {
		return p, err
	}
	// All validation occurs before writing. Restore local worktree/index on any
	// pre-publication failure; remote publication remains one conditional ref update.
	committed := false
	defer func() {
		if !committed {
			_ = wt.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: head.Hash()})
		}
	}()
	for _, d := range p.Diffs {
		file, rel, err := resolveWorkspaceFile(g.workspace, d.File)
		if err != nil {
			return p, err
		}
		stat, err := os.Stat(file)
		if err != nil {
			return p, err
		}
		if err := os.WriteFile(file, []byte(updated[d.File]), stat.Mode().Perm()); err != nil {
			return p, err
		}
		if _, err := wt.Add(rel); err != nil {
			return p, err
		}
	}
	hash, err := wt.Commit("Apply ChangeSet "+safeCommitID(p.ID), &git.CommitOptions{Author: commitAuthor()})
	if err != nil {
		return p, err
	}
	p.Commit = hash.String()
	p.State = "publishing"
	if err := g.jobStore.savePlan(p, false); err != nil {
		return p, err
	}
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ref := "refs/heads/" + branch
	err = g.repo.PushContext(pushCtx, &git.PushOptions{RemoteName: "origin", ClientOptions: g.authOpts, RefSpecs: []config.RefSpec{config.RefSpec(ref + ":" + ref)}, RequireRemoteRefs: []config.RefSpec{config.RefSpec(p.BaseRevision + ":" + ref)}})
	if err != nil {
		p.State = "unknown"
		p.Error = "Push did not confirm publication; inspect remote or retry apply to reconcile"
		if saveErr := g.jobStore.savePlan(p, false); saveErr != nil {
			return p, saveErr
		}
		return p, fmt.Errorf("%w: %s", controller.ErrConflict, p.Error)
	}
	committed = true
	p.State = "published"
	p.Error = ""
	return p, g.jobStore.savePlan(p, false)
}
func sameMutations(a, b controller.Plan) bool {
	if len(a.Mutations) != len(b.Mutations) || len(a.Diffs) != len(b.Diffs) {
		return false
	}
	for i := range a.Mutations {
		if a.Mutations[i] != b.Mutations[i] {
			return false
		}
	}
	for i := range a.Diffs {
		if a.Diffs[i] != b.Diffs[i] {
			return false
		}
	}
	return true
}
func safeCommitID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}
func (g *gitManager) reachable(commit string) bool {
	head, err := g.repo.Head()
	if err != nil {
		return false
	}
	iter, err := g.repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return false
	}
	defer iter.Close()
	for i := 0; i < 10000; i++ {
		c, err := iter.Next()
		if err != nil {
			return false
		}
		if c.Hash == plumbing.NewHash(commit) {
			return true
		}
	}
	return false
}

func (g *gitManager) work(job Job) (JobOutcome, error) {
	if err := ValidateUpdateJob(job); err != nil {
		return "", err
	}
	g.operationMu.Lock()
	defer g.operationMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sum := sha256.Sum256([]byte(job.ID))
	id := "job-" + hex.EncodeToString(sum[:])
	if _, err := g.jobStore.GetPlan(id); errors.Is(err, controller.ErrNotFound) {
		if _, err := g.originURL(); err != nil {
			return "", err
		}
		if err := g.syncRepository(); err != nil {
			return "", err
		}
		head, err := g.repo.Head()
		if err != nil {
			return "", err
		}
		if _, err := g.jobStore.db.Exec(`INSERT INTO job_baselines(id,revision) VALUES(?,?) ON CONFLICT DO NOTHING`, job.ID, head.Hash().String()); err != nil {
			return "", err
		}
		var baseline string
		if err := g.jobStore.db.QueryRow(`SELECT revision FROM job_baselines WHERE id=?`, job.ID).Scan(&baseline); err != nil {
			return "", err
		}
		if baseline != head.Hash().String() {
			return "", fmt.Errorf("%w: retry baseline changed; submit a new intent", controller.ErrConflict)
		}
		if job.File != "" {
			if _, _, err := resolveWorkspaceFile(g.workspace, job.File); err != nil {
				return "", err
			}
		}
	} else if err != nil {
		return "", err
	}
	change := controller.Change{Image: job.Image, Tag: job.Tag, File: job.File}
	if !job.Force {
		allowed := map[string]bool{}
		for _, env := range strings.Split(os.Getenv("AUTOMATION_ENVIRONMENTS"), ",") {
			if e := strings.TrimSpace(env); e != "" {
				allowed[e] = true
			}
		}
		if len(allowed) > 0 {
			model, _, err := g.snapshot()
			if err != nil {
				return "", err
			}
			if len(model.Diagnostics) > 0 {
				return "", fmt.Errorf("%w: repository has parser diagnostics", controller.ErrInvalid)
			}
			found := false
			seen := map[string]bool{}
			for _, u := range model.Uses {
				if controller.ImageName(u.SourceImage) != controller.ImageName(job.Image) && controller.ImageName(u.EffectiveImage) != controller.ImageName(job.Image) {
					continue
				}
				found = true
				if allowed[u.Environment] && !seen[u.Environment] {
					change.Environments = append(change.Environments, u.Environment)
					seen[u.Environment] = true
				}
			}
			if !found {
				return OutcomeNoMatch, nil
			}
			if len(change.Environments) == 0 {
				return OutcomeSkippedPolicy, nil
			}
		}
	}
	p, err := g.preview(ctx, controller.Intent{ID: id, Changes: []controller.Change{change}})
	if errors.Is(err, controller.ErrNoMatch) {
		return OutcomeNoMatch, nil
	}
	if err != nil {
		return "", err
	}
	p, err = g.applyPlan(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if p.State == "already_satisfied" {
		return OutcomeAlreadySatisfied, nil
	}
	return OutcomePublished, nil
}
