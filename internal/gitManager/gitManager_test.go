package gitManager

import (
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
