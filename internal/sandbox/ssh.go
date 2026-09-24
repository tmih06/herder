// Package sandbox — SSH transport for worker sandboxes (issue #19): every
// worker container runs an in-container herdr server reached over SSH, and
// SSH reaches the container through `ProxyCommand docker exec -i <ctr>
// /usr/sbin/sshd -i` — sshd in inetd mode, so no port is ever published and
// the same transport maps 1:1 onto `kubectl exec -i <pod> -- sshd -i` for a
// future provider.
//
// Why: the host pane ↔ container split was a shim — Herdr detected the
// symlink's comm, not the agent, and every interaction proxied through
// `docker exec`. A container-local herdr server gives the agent real
// detection, per-container networking, and a control surface that governs
// only its own session.
// Approach: the controller owns one SSH keypair under <statedir>/ssh; only
// the public key enters the sandbox. Per-task SSH config blocks live in
// <statedir>/ssh/config.d/, included from ~/.ssh/config by one managed
// Include line — herdr's `machine add` and `--machine` commands resolve
// targets through the ordinary ssh config chain, so the block must sit
// where OpenSSH reads it (verified against herdr 0.9.1: `machine add`
// runs ssh -F <managed> which Includes ~/.ssh/config; `--machine` runs
// plain ssh with StrictHostKeyChecking=yes, so accept-new seeds the
// per-task known_hosts during add).
// Inputs: the state dir (SSH assets) and the task's container name.
// Flow: EnsureKeypair -> EnsureHostKey -> WriteConfig -> EnsureSSHInclude
// -> inject authorized_keys + host key -> machine add.
// Returns: SSHAssets carrying every path the provisioner needs.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Container-side layout: the worker user's home lives under /tmp because
// the container uid is dynamic — /tmp is world-writable, so no chown is
// needed (CAP_CHOWN is dropped) and the home survives only as long as the
// container does. sshd resolves it through the passwd entry provisioned
// into the container.
const (
	// ContainerHome is the worker user's home inside the sandbox.
	ContainerHome = "/tmp/herder-home"
	// ContainerSSHDir holds authorized_keys and the sshd host key.
	ContainerSSHDir = ContainerHome + "/.ssh"
	// ContainerHostKeyPath is the sshd host key inside the sandbox.
	ContainerHostKeyPath = ContainerSSHDir + "/ssh_host_ed25519_key"
	// ContainerAuthorizedKeys is the sshd authorized_keys path.
	ContainerAuthorizedKeys = ContainerSSHDir + "/authorized_keys"
	// ContainerWorkspace is the fixed mount point of the task checkout.
	ContainerWorkspace = "/workspace"
	// SSHUser is the login name provisioned into the container's passwd.
	SSHUser = "worker"
)

// SSHAssets is every controller-side SSH path for one task.
type SSHAssets struct {
	// StateDir is <dbdir> — parent of the ssh directory.
	StateDir string
	// Container is the docker container name and the SSH Host alias.
	Container string
}

// SSHDir returns <statedir>/ssh — the controller's SSH asset root.
func (a SSHAssets) SSHDir() string { return filepath.Join(a.StateDir, "ssh") }

// ConfigDir returns <statedir>/ssh/config.d — one file per task Host block.
func (a SSHAssets) ConfigDir() string { return filepath.Join(a.SSHDir(), "config.d") }

// IdentityFile is the controller's SSH private key (never enters a sandbox).
func (a SSHAssets) IdentityFile() string { return filepath.Join(a.SSHDir(), "id_ed25519") }

// PublicKeyFile is the public half injected as authorized_keys.
func (a SSHAssets) PublicKeyFile() string { return a.IdentityFile() + ".pub" }

// HostKeyFile is the per-task sshd host key injected into the container:
// stable across container recreates so known_hosts stays valid.
func (a SSHAssets) HostKeyFile() string {
	return filepath.Join(a.SSHDir(), "hostkey_"+a.Container)
}

// ConfigFile is this task's dedicated ssh config block.
func (a SSHAssets) ConfigFile() string {
	return filepath.Join(a.ConfigDir(), a.Container)
}

// KnownHostsFile is this task's dedicated known_hosts: per-task so a
// recreated container never collides with a stale global entry and
// teardown deletes the file outright.
func (a SSHAssets) KnownHostsFile() string {
	return filepath.Join(a.SSHDir(), "known_hosts_"+a.Container)
}

// ProxyCommandLine is the ssh ProxyCommand that reaches sshd inside the
// container in inetd mode — no published port, no sshd daemon. The -o
// overrides keep the image generic: PAM off (the worker uid has no PAM
// stack), no pid file (unwritable as non-root), public-key only.
func (a SSHAssets) ProxyCommandLine() string {
	return fmt.Sprintf("docker exec -i %s /usr/sbin/sshd -i -e -h %s "+
		"-o UsePAM=no -o PidFile=none -o PasswordAuthentication=no -o PermitRootLogin=no",
		a.Container, ContainerHostKeyPath)
}

// ConfigBlock renders this task's Host block: the pure, unit-testable
// core of SSH config generation (issue #19: generation is a pure
// function). Host-key policy is accept-new against a per-task
// known_hosts — never touching the user's global known_hosts and never
// disabling verification.
func (a SSHAssets) ConfigBlock() string {
	return fmt.Sprintf(`Host %s
    User %s
    ProxyCommand %s
    IdentityFile %s
    IdentitiesOnly yes
    StrictHostKeyChecking accept-new
    UserKnownHostsFile %s
`, a.Container, SSHUser, a.ProxyCommandLine(), a.IdentityFile(), a.KnownHostsFile())
}

// WriteConfig writes this task's Host block into <statedir>/ssh/config.d/,
// creating the directory. Idempotent: same content, same file.
func (a SSHAssets) WriteConfig() error {
	if err := os.MkdirAll(a.ConfigDir(), 0o700); err != nil {
		return fmt.Errorf("sandbox: create ssh config dir: %w", err)
	}
	if err := os.WriteFile(a.ConfigFile(), []byte(a.ConfigBlock()), 0o600); err != nil {
		return fmt.Errorf("sandbox: write ssh config: %w", err)
	}
	return nil
}

// RemoveConfig deletes this task's Host block, known_hosts, and host key:
// teardown leaves no per-task SSH state behind. Missing files are fine.
func (a SSHAssets) RemoveConfig() error {
	for _, path := range []string{a.ConfigFile(), a.KnownHostsFile(), a.HostKeyFile()} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("sandbox: remove %s: %w", path, err)
		}
	}
	return nil
}

// IncludeLine is the single directive appended to ~/.ssh/config so
// OpenSSH — and therefore herdr's machine commands — resolves worker
// Host blocks. Globbed: per-task files drop in and out freely.
func (a SSHAssets) IncludeLine() string {
	return "Include " + a.ConfigDir() + "/*"
}

// EnsureSSHInclude makes the controller's config.d directory visible to
// OpenSSH by inserting one Include line into ~/.ssh/config, creating the
// file when absent. The line must land in global scope — before the
// first Host/Match block — because an Include inside a Host block is
// conditional on that host and would hide every worker block (verified:
// appending after a trailing Host block left `ssh -G` resolving no Host
// entry). The user's config is never rewritten beyond that one line.
func (a SSHAssets) EnsureSSHInclude() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("sandbox: resolve home for ssh config: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	userConfig := filepath.Join(sshDir, "config")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("sandbox: create ~/.ssh: %w", err)
	}
	line := a.IncludeLine()
	block := "# Herder worker sandboxes (managed; safe to remove)\n" + line + "\n"
	raw, err := os.ReadFile(userConfig)
	switch {
	case err == nil:
		lines := strings.Split(string(raw), "\n")
		for _, existing := range lines {
			if strings.TrimSpace(existing) == line {
				return nil
			}
		}
		// Insert before the first Host/Match block so the Include is
		// global; with no blocks, append at the end (still global).
		at := len(lines)
		for i, l := range lines {
			t := strings.ToLower(strings.TrimSpace(l))
			if strings.HasPrefix(t, "host ") || strings.HasPrefix(t, "match ") {
				at = i
				break
			}
		}
		out := make([]string, 0, len(lines)+3)
		out = append(out, lines[:at]...)
		out = append(out, strings.Split(strings.TrimRight(block, "\n"), "\n")...)
		out = append(out, lines[at:]...)
		if err := os.WriteFile(userConfig, []byte(strings.Join(out, "\n")), 0o600); err != nil {
			return fmt.Errorf("sandbox: update ~/.ssh/config: %w", err)
		}
		return nil
	case os.IsNotExist(err):
		if err := os.WriteFile(userConfig, []byte(block), 0o600); err != nil {
			return fmt.Errorf("sandbox: create ~/.ssh/config: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("sandbox: read ~/.ssh/config: %w", err)
	}
}

// KeygenArgv builds one ssh-keygen invocation: ed25519, no passphrase,
// quiet, overwriting nothing (ssh-keygen refuses existing files without
// -f confirmation, so callers check existence first).
func KeygenArgv(path string) []string {
	return []string{"-t", "ed25519", "-N", "", "-q", "-f", path}
}
