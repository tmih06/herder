package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigBlockRendersHostEntry proves the generated Host block carries
// every directive herdr's ssh chain needs: the container name as alias,
// the provisioned worker login, the docker-exec ProxyCommand reaching
// sshd -i, the controller identity, and accept-new host-key policy pinned
// to a per-task known_hosts.
func TestConfigBlockRendersHostEntry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stateDir  string
		container string
	}{
		{"standard", "/state", "herder-task_abc123"},
		{"nested state dir", "/var/lib/herder/db", "herder-task_xyz999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := SSHAssets{StateDir: tc.stateDir,  Container: tc.container}
			block := a.ConfigBlock()
			for _, want := range []string{
				"Host " + tc.container,
				"User " + SSHUser,
				"ProxyCommand docker exec -i " + tc.container +
					" /usr/sbin/sshd -i -e -h " + ContainerHostKeyPath +
					" -o UsePAM=no -o PidFile=none -o PasswordAuthentication=no -o PermitRootLogin=no",
				"IdentityFile " + filepath.Join(tc.stateDir, "ssh", "id_ed25519"),
				"IdentitiesOnly yes",
				"StrictHostKeyChecking accept-new",
				"UserKnownHostsFile " + filepath.Join(tc.stateDir, "ssh", "known_hosts_"+tc.container),
			} {
				if !strings.Contains(block, want) {
					t.Errorf("config block missing %q:\n%s", want, block)
				}
			}
			// Host-key verification stays on: never the bypass value.
			if strings.Contains(block, "StrictHostKeyChecking no") {
				t.Errorf("config block must never disable host-key checks:\n%s", block)
			}
		})
	}
}

// TestIncludeLineGlobsConfigDir proves the managed Include directive
// points at the per-task config.d glob so blocks drop in and out freely.
func TestIncludeLineGlobsConfigDir(t *testing.T) {
	a := SSHAssets{StateDir: "/state"}
	want := "Include /state/ssh/config.d/*"
	if got := a.IncludeLine(); got != want {
		t.Errorf("IncludeLine = %q, want %q", got, want)
	}
}

// TestWriteConfigRemoveConfigRoundTrip proves WriteConfig materializes
// the Host block under config.d and RemoveConfig deletes the block plus
// the per-task known_hosts and host key — and tolerates them missing.
func TestWriteConfigRemoveConfigRoundTrip(t *testing.T) {
	a := SSHAssets{StateDir: t.TempDir(), Container: "herder-task_abc123"}
	if err := a.WriteConfig(); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(a.ConfigFile())
	if err != nil {
		t.Fatalf("config file must exist after WriteConfig: %v", err)
	}
	if string(raw) != a.ConfigBlock() {
		t.Errorf("written config = %q, want ConfigBlock output", raw)
	}
	// Idempotent: a second write converges to the same file.
	if err := a.WriteConfig(); err != nil {
		t.Fatalf("second WriteConfig: %v", err)
	}
	for _, path := range []string{a.KnownHostsFile(), a.HostKeyFile()} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	if err := a.RemoveConfig(); err != nil {
		t.Fatalf("RemoveConfig: %v", err)
	}
	for _, path := range []string{a.ConfigFile(), a.KnownHostsFile(), a.HostKeyFile()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("RemoveConfig must delete %s, stat err = %v", path, err)
		}
	}
	// Missing files are fine: teardown is a no-op, not an error.
	if err := a.RemoveConfig(); err != nil {
		t.Errorf("RemoveConfig on absent files must be a no-op, got %v", err)
	}
}

// TestEnsureSSHInclude proves the managed Include line reaches
// ~/.ssh/config exactly once: the file is created when absent, appended
// without disturbing user content, and never duplicated.
func TestEnsureSSHInclude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := SSHAssets{StateDir: t.TempDir()}
	userConfig := filepath.Join(home, ".ssh", "config")

	t.Run("creates config when absent", func(t *testing.T) {
		if err := a.EnsureSSHInclude(); err != nil {
			t.Fatalf("EnsureSSHInclude: %v", err)
		}
		raw, err := os.ReadFile(userConfig)
		if err != nil {
			t.Fatalf("~/.ssh/config must be created: %v", err)
		}
		if !strings.Contains(string(raw), a.IncludeLine()) {
			t.Errorf("config must contain %q, got:\n%s", a.IncludeLine(), raw)
		}
	})

	t.Run("appends once and keeps user content", func(t *testing.T) {
		userBlock := "Host personal\n    HostName example.com\n"
		if err := os.WriteFile(userConfig, []byte(userBlock), 0o600); err != nil {
			t.Fatalf("seed user config: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := a.EnsureSSHInclude(); err != nil {
				t.Fatalf("EnsureSSHInclude call %d: %v", i, err)
			}
		}
		raw, err := os.ReadFile(userConfig)
		if err != nil {
			t.Fatalf("read config: %v", err)
		}
		if !strings.HasPrefix(string(raw), userBlock) {
			t.Errorf("user config must be preserved, got:\n%s", raw)
		}
		if n := strings.Count(string(raw), a.IncludeLine()); n != 1 {
			t.Errorf("Include line must appear once after repeated calls, got %d:\n%s", n, raw)
		}
	})
}
