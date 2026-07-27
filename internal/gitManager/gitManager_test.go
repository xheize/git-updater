package gitManager

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeGitURL(t *testing.T) {
	tests := []struct {
		name       string
		rawURL     string
		authMethod string
		expected   string
	}{
		{
			name:       "HTTPS to SSH (GitHub)",
			rawURL:     "https://github.com/xheize/git-updater.git",
			authMethod: "ssh",
			expected:   "git@github.com:xheize/git-updater.git",
		},
		{
			name:       "HTTP to SSH (GitLab)",
			rawURL:     "http://gitlab.com/group/project.git",
			authMethod: "ssh",
			expected:   "git@gitlab.com:group/project.git",
		},
		{
			name:       "SSH to HTTPS (GitHub SCP format)",
			rawURL:     "git@github.com:xheize/git-updater.git",
			authMethod: "http",
			expected:   "https://github.com/xheize/git-updater.git",
		},
		{
			name:       "SSH to HTTPS (GitLab ssh:// format)",
			rawURL:     "ssh://git@gitlab.com/group/project.git",
			authMethod: "http",
			expected:   "https://gitlab.com/group/project.git",
		},
		{
			name:       "SSH to SSH (no change)",
			rawURL:     "git@github.com:xheize/git-updater.git",
			authMethod: "ssh",
			expected:   "git@github.com:xheize/git-updater.git",
		},
		{
			name:       "HTTPS to HTTPS (no change)",
			rawURL:     "https://github.com/xheize/git-updater.git",
			authMethod: "http",
			expected:   "https://github.com/xheize/git-updater.git",
		},
		{
			name:       "Empty URL",
			rawURL:     "",
			authMethod: "ssh",
			expected:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := normalizeGitURL(tt.rawURL, tt.authMethod)
			if actual != tt.expected {
				t.Errorf("normalizeGitURL(%q, %q) = %q; expected %q", tt.rawURL, tt.authMethod, actual, tt.expected)
			}
		})
	}
}

func TestGetBaseImageName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Standard registry with tag",
			input:    "registry.xheize.cc/app/nginx:1.20",
			expected: "registry.xheize.cc/app/nginx",
		},
		{
			name:     "Registry with port and tag",
			input:    "registry.xheize.cc:5000/app/nginx:1.20",
			expected: "registry.xheize.cc:5000/app/nginx",
		},
		{
			name:     "Registry with port, no tag",
			input:    "registry.xheize.cc:5000/app/nginx",
			expected: "registry.xheize.cc:5000/app/nginx",
		},
		{
			name:     "Simple image with tag",
			input:    "nginx:latest",
			expected: "nginx",
		},
		{
			name:     "Simple image, no tag",
			input:    "nginx",
			expected: "nginx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := getBaseImageName(tt.input)
			if actual != tt.expected {
				t.Errorf("getBaseImageName(%q) = %q; expected %q", tt.input, actual, tt.expected)
			}
		})
	}
}

func TestResolveWorkspaceFile(t *testing.T) {
	workspace := t.TempDir()
	validFile := filepath.Join(workspace, "deployments", "web.yaml")
	if err := os.MkdirAll(filepath.Dir(validFile), 0755); err != nil {
		t.Fatalf("create test directory: %v", err)
	}
	if err := os.WriteFile(validFile, []byte("image: nginx:1.0"), 0644); err != nil {
		t.Fatalf("create test file: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "valid relative YAML path", path: filepath.Join("deployments", "web.yaml")},
		{name: "parent directory traversal", path: filepath.Join("..", "outside.yaml"), wantErr: true},
		{name: "absolute path", path: validFile, wantErr: true},
		{name: "non-YAML extension", path: "deployments/web.txt", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotRelativePath, err := resolveWorkspaceFile(workspace, tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWorkspaceFile returned an error: %v", err)
			}
			if gotPath != validFile {
				t.Errorf("file path = %q, expected %q", gotPath, validFile)
			}
			if gotRelativePath != filepath.Join("deployments", "web.yaml") {
				t.Errorf("relative path = %q", gotRelativePath)
			}
		})
	}
}

func TestResolveWorkspaceFileRejectsSymlink(t *testing.T) {
	workspace := t.TempDir()
	outsideFile := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outsideFile, []byte("image: nginx:1.0"), 0644); err != nil {
		t.Fatalf("create outside file: %v", err)
	}

	linkPath := filepath.Join(workspace, "linked.yaml")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("symbolic links are unavailable in this environment: %v", err)
	}

	_, _, err := resolveWorkspaceFile(workspace, "linked.yaml")
	if err == nil {
		t.Fatal("expected symbolic link to be rejected")
	}
}
