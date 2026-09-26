package gitManager

import (
	"errors"
	"path"
	"strings"
	"unicode"

	"github.com/distribution/reference"
)

// ValidateUpdateJob checks syntax without touching Git or the filesystem.
// Image is a repository name; Tag is supplied separately. Digest mutation is
// not supported by this API, so accepting a digest here would be ambiguous.
func ValidateUpdateJob(job Job) error {
	ref, err := reference.Parse(job.Image)
	if err != nil {
		return errors.New("image must be a valid repository name without a tag or digest")
	}
	named, ok := ref.(reference.Named)
	if !ok || !reference.IsNameOnly(named) {
		return errors.New("image must be a repository name without a tag or digest")
	}
	if _, err := reference.WithTag(named, job.Tag); err != nil {
		return errors.New("tag must be a valid container image tag (1-128 characters)")
	}
	if job.File != "" {
		return ValidateUpdatePath(job.File)
	}
	return nil
}

// ValidateUpdatePath uses a portable repository-relative path contract. Actual
// file existence and symlink checks remain in resolveWorkspaceFile at execution.
func ValidateUpdatePath(file string) error {
	if strings.TrimSpace(file) != file || strings.ContainsAny(file, "\\:") || path.IsAbs(file) ||
		strings.IndexFunc(file, unicode.IsControl) >= 0 {
		return errors.New("file must be a repository-relative path using forward slashes")
	}
	for _, part := range strings.Split(file, "/") {
		if part == ".." || strings.EqualFold(part, ".git") {
			return errors.New("file must not traverse parent directories or Git metadata")
		}
	}
	ext := strings.ToLower(path.Ext(file))
	if ext != ".yaml" && ext != ".yml" {
		return errors.New("file must name a YAML file")
	}
	return nil
}
