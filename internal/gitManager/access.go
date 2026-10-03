package gitManager

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
)

// Credentials are captured in authOpts at startup. Restart after changing them;
// successful access probes are intentionally not trusted across process restarts.
func (g *gitManager) initializeAccess() {
	if os.Getenv("CONTROLLER_LOCAL_MODE") == "true" {
		return
	}
	branch, err := g.BranchName()
	if err == nil {
		err = g.ensureGitAccess(context.Background(), branch)
	}
	if err != nil {
		g.accessError = err.Error()
		log.Printf("Initial Git access validation: %v", err)
	}
}

// Caller serializes this with operationMu (or calls it before serving requests).
// The durable pending record lets a new process finish cleanup after a crash.
func (g *gitManager) ensureGitAccess(ctx context.Context, branch string) (result error) {
	defer func() {
		if result != nil {
			g.accessError = result.Error()
		}
	}()
	raw, err := g.originURL()
	if err != nil {
		return err
	}
	if raw == g.accessOrigin && branch == g.accessBranch && !g.accessCheckedAt.IsZero() {
		return nil
	}
	if _, err = g.jobStore.db.Exec(`CREATE TABLE IF NOT EXISTS git_access_probes (origin TEXT PRIMARY KEY, ref TEXT NOT NULL, revision TEXT NOT NULL)`); err != nil {
		return err
	}
	var pending, revision string
	err = g.jobStore.db.QueryRow(`SELECT ref,revision FROM git_access_probes WHERE origin=?`, raw).Scan(&pending, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if pending != "" {
		if err = g.cleanupAccessProbe(raw, pending, revision); err != nil {
			return err
		}
	}
	refs, err := g.accessRefs(ctx)
	if err != nil {
		return fmt.Errorf("Git read verification failed")
	}
	hash, ok := refs[plumbing.NewBranchReferenceName(branch)]
	if !ok {
		return fmt.Errorf("target branch does not exist")
	}
	if _, err = g.repo.CommitObject(hash); err != nil {
		return fmt.Errorf("remote branch changed; fetch and retry verification")
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	ref := "refs/heads/git-updater-verify/" + hex.EncodeToString(nonce)
	if _, exists := refs[plumbing.ReferenceName(ref)]; exists {
		return fmt.Errorf("verification ref already exists")
	}
	if _, err = g.jobStore.db.Exec(`INSERT INTO git_access_probes(origin,ref,revision) VALUES(?,?,?)`, raw, ref, hash.String()); err != nil {
		return err
	}
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = g.repo.PushContext(pushCtx, &git.PushOptions{RemoteName: "origin", ClientOptions: g.authOpts, RefSpecs: []config.RefSpec{config.RefSpec(hash.String() + ":" + ref)}, ForceWithLease: &git.ForceWithLease{RefName: plumbing.ReferenceName(ref), Hash: plumbing.ZeroHash}})
	cancel()
	// Independent cleanup context: cancellation must not strand a probe branch.
	cleanupErr := g.cleanupAccessProbe(raw, ref, hash.String())
	if cleanupErr != nil {
		return cleanupErr
	}
	if err != nil {
		return fmt.Errorf("Git write verification failed (probe create); target access not granted")
	}
	g.accessOrigin, g.accessBranch, g.accessCheckedAt, g.accessError = raw, branch, time.Now().UTC(), ""
	return nil
}

func (g *gitManager) accessRefs(ctx context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	remote, err := g.repo.Remote("origin")
	if err != nil {
		return nil, err
	}
	timed, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	refs, err := remote.ListContext(timed, &git.ListOptions{ClientOptions: g.authOpts})
	if err != nil {
		return nil, err
	}
	out := map[plumbing.ReferenceName]plumbing.Hash{}
	for _, r := range refs {
		if r.Type() == plumbing.HashReference {
			out[r.Name()] = r.Hash()
		}
	}
	return out, nil
}

func (g *gitManager) cleanupAccessProbe(origin, ref, revision string) error {
	nonce, nonceErr := hex.DecodeString(strings.TrimPrefix(ref, "refs/heads/git-updater-verify/"))
	if !strings.HasPrefix(ref, "refs/heads/git-updater-verify/") || nonceErr != nil || len(nonce) != 16 || !plumbing.IsHash(revision) {
		return fmt.Errorf("invalid pending verification record; manual inspection required")
	}
	ctx := context.Background()
	refs, err := g.accessRefs(ctx)
	if err != nil {
		return fmt.Errorf("cannot confirm probe cleanup: %s", ref)
	}
	if hash, exists := refs[plumbing.ReferenceName(ref)]; exists {
		if hash.String() != revision {
			return fmt.Errorf("probe ref changed; refusing deletion: %s", ref)
		}
		timed, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = g.repo.PushContext(timed, &git.PushOptions{RemoteName: "origin", ClientOptions: g.authOpts, RefSpecs: []config.RefSpec{config.RefSpec(":" + ref)}, RequireRemoteRefs: []config.RefSpec{config.RefSpec(revision + ":" + ref)}})
		cancel()
		if err != nil {
			return fmt.Errorf("probe deletion failed; cleanup required: %s", ref)
		}
		refs, err = g.accessRefs(ctx)
		if err != nil {
			return fmt.Errorf("cannot confirm probe deletion: %s", ref)
		}
		if _, exists = refs[plumbing.ReferenceName(ref)]; exists {
			return fmt.Errorf("probe still exists: %s", ref)
		}
	}
	_, err = g.jobStore.db.Exec(`DELETE FROM git_access_probes WHERE origin=? AND ref=?`, origin, ref)
	return err
}
