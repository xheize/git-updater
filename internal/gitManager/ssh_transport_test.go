package gitManager

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	sshconfig "github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestSSHTransportUsesConfiguredKnownHosts(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		mismatch, alias, explicitPort bool
	}{
		{name: "matching"},
		{name: "mismatched", mismatch: true},
		{name: "alias", alias: true},
		{name: "alias-explicit-port", alias: true, explicitPort: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			_, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.NewSignerFromKey(privateKey)
			if err != nil {
				t.Fatal(err)
			}
			der, err := x509.MarshalPKCS8PrivateKey(privateKey)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			cloneURL := "ssh://git@" + listener.Addr().String() + "/repo"
			if tc.alias {
				host, port, err := net.SplitHostPort(listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				configPort := port
				cloneURL = "ssh://git@review-alias/repo"
				if tc.explicitPort {
					configPort = "1"
					cloneURL = "ssh://git@review-alias:" + port + "/repo"
				}
				configPath := filepath.Join(root, "ssh_config")
				if err := os.WriteFile(configPath, []byte(fmt.Sprintf("Host review-alias\n  HostName %s\n  Port %s\n", host, configPort)), 0600); err != nil {
					t.Fatal(err)
				}
				previous := sshconfig.DefaultUserSettings
				settings := &sshconfig.UserSettings{}
				settings.ConfigFinder(func() string { return configPath })
				sshconfig.DefaultUserSettings = settings
				t.Cleanup(func() { sshconfig.DefaultUserSettings = previous })
			}
			serverConfig := &ssh.ServerConfig{NoClientAuth: true}
			serverConfig.AddHostKey(signer)
			handshake := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					handshake <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				server, _, _, err := ssh.NewServerConn(conn, serverConfig)
				if server != nil {
					server.Close()
				}
				handshake <- err
			}()
			registeredKey := signer.PublicKey()
			if tc.mismatch {
				pub, _, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				registeredKey, err = ssh.NewPublicKey(pub)
				if err != nil {
					t.Fatal(err)
				}
			}
			hosts := filepath.Join(root, "known_hosts")
			if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{listener.Addr().String()}, registeredKey)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_AUTH_METHOD", "ssh")
			t.Setenv("GIT_SSH_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
			t.Setenv("GIT_SSH_KNOWN_HOSTS_FILE", hosts)
			// A custom file must work without any legacy/default known_hosts file.
			t.Setenv("SSH_KNOWN_HOSTS", filepath.Join(root, "missing-default-hosts"))
			opts, err := getGitAuth()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// The server deliberately closes after the handshake; it does not serve Git.
			_, cloneErr := git.PlainCloneContext(ctx, filepath.Join(root, "clone"), &git.CloneOptions{URL: cloneURL, ClientOptions: opts})
			select {
			case err := <-handshake:
				if !tc.mismatch && err != nil {
					t.Fatalf("matching key rejected: handshake=%v clone=%v", err, cloneErr)
				}
				if tc.mismatch && err == nil {
					t.Fatal("mismatched host key was accepted")
				}
			case <-ctx.Done():
				t.Fatalf("SSH handshake never completed: clone=%v", cloneErr)
			}
		})
	}
}
