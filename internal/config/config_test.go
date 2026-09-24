package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Load the checked-in example config: the daemon's happy path must accept it.
func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load("../../examples/herder.yaml")
	if err != nil {
		t.Fatalf("Load(example) = %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:8787" {
		t.Errorf("Listen = %q, want 127.0.0.1:8787", cfg.Server.Listen)
	}
	if len(cfg.Repositories) != 1 || len(cfg.Agents) != 1 {
		t.Errorf("want 1 repository and 1 agent, got %d/%d",
			len(cfg.Repositories), len(cfg.Agents))
	}
}

// The checked-in example and the `herder init` builtin must stay identical.
func TestExampleMatchesBuiltin(t *testing.T) {
	raw, err := os.ReadFile("../../examples/herder.yaml")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if string(raw) != ExampleYAML {
		t.Error("examples/herder.yaml drifts from config.ExampleYAML; update both")
	}
}

// writeConfig writes a config body to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herder.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validBody = `
server:
  listen: 127.0.0.1:8787
database:
  path: /tmp/herder-test/herder.db
herdr:
  mode: socket
scheduler:
  max_workers: 2
repositories:
  acme/web:
    enabled: true
    trigger:
      labels: [agent-ready]
    agent:
      default: codex-default
    sandbox:
      provider: docker
agents:
  codex-default:
    kind: codex
    timeout: 2h
`

// The issue #6 gate knobs must parse: forbidden path globs, the clean-tree
// switch, and the delivery label path.
func TestValidationDeliveryConfig(t *testing.T) {
	// validBody ends inside the agents map; splice the repo keys under the
	// repository entry instead of appending at top level.
	body := strings.Replace(validBody, "      provider: docker\n",
		`      provider: docker
    validation:
      commands: [go test ./...]
      forbidden_changes: [".github/workflows/**"]
      require_clean_git: false
    delivery:
      create_pr: true
      labels:
        running: ci-running
        review: ci-review
        completed: ci-done
`, 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	repo := cfg.Repositories["acme/web"]
	if len(repo.Validation.ForbiddenChanges) != 1 ||
		repo.Validation.ForbiddenChanges[0] != ".github/workflows/**" {
		t.Errorf("forbidden_changes = %v", repo.Validation.ForbiddenChanges)
	}
	if repo.Validation.CleanTreeRequired() {
		t.Error("require_clean_git: false must disable the clean-tree check")
	}
	labels := repo.Delivery.LabelSet()
	if labels.Running != "ci-running" || labels.Review != "ci-review" || labels.Completed != "ci-done" {
		t.Errorf("labels = %+v, want the configured stage names", labels)
	}
}

// Omitted gate knobs must default safe: clean tree required, and the SPEC
// section 35 label path agent-running -> agent-review -> completed.
func TestValidationDeliveryDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, validBody))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	repo := cfg.Repositories["acme/web"]
	if !repo.Validation.CleanTreeRequired() {
		t.Error("require_clean_git must default to true")
	}
	labels := repo.Delivery.LabelSet()
	if labels.Running != "agent-running" || labels.Review != "agent-review" ||
		labels.Completed != "completed" {
		t.Errorf("default labels = %+v, want the spec stage names", labels)
	}
}

// Bad gate config fails at load: escaping globs and unprintable labels.
func TestValidateBadGateConfig(t *testing.T) {
	for name, fragment := range map[string]string{
		"escaping glob": `    validation:
      forbidden_changes: ["../outside/**"]
`,
		"absolute glob": `    validation:
      forbidden_changes: ["/etc/passwd"]
`,
		"bad label": `    delivery:
      labels:
        review: "not a label!"
`,
	} {
		body := strings.Replace(validBody, "      provider: docker\n",
			"      provider: docker\n"+fragment, 1)
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: Load must reject the config", name)
		}
	}
}

// TestValidateBadTrigger Empty trigger labels fail naming trigger/labels.
func TestValidateBadTrigger(t *testing.T) {
	bad := strings.Replace(validBody, "labels: [agent-ready]", "labels: []", 1)
	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected error for empty trigger labels, got nil")
	}
	if !strings.Contains(err.Error(), "trigger") && !strings.Contains(err.Error(), "label") {
		t.Errorf("error should name trigger/labels, got: %v", err)
	}
}

// TestValidateUnknownAgentProfile Dangling agent references fail naming the profile.
func TestValidateUnknownAgentProfile(t *testing.T) {
	bad := strings.Replace(validBody, "default: codex-default", "default: ghost-profile", 1)
	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected error for unknown agent profile, got nil")
	}
	if !strings.Contains(err.Error(), "ghost-profile") {
		t.Errorf("error should name the unknown profile, got: %v", err)
	}
}

// TestValidateUnknownSandboxProvider Unknown providers fail naming the value and want-list.
func TestValidateUnknownSandboxProvider(t *testing.T) {
	bad := strings.Replace(validBody, "provider: docker", "provider: teleport", 1)
	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected error for unknown sandbox provider, got nil")
	}
	if !strings.Contains(err.Error(), "teleport") || !strings.Contains(err.Error(), "docker") {
		t.Errorf("error should name the bad provider and allowed values, got: %v", err)
	}
}

// TestValidateUnknownAgentKind Unknown agent kinds fail naming the kind.
func TestValidateUnknownAgentKind(t *testing.T) {
	bad := strings.Replace(validBody, "kind: codex", "kind: skynet", 1)
	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected error for unknown agent kind, got nil")
	}
	if !strings.Contains(err.Error(), "skynet") {
		t.Errorf("error should name the bad kind, got: %v", err)
	}
}

// TestValidateBadListen Unparsable listen addresses fail naming listen.
func TestValidateBadListen(t *testing.T) {
	bad := strings.Replace(validBody, "127.0.0.1:8787", "not-an-address", 1)
	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected error for bad listen address, got nil")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error should name listen, got: %v", err)
	}
}

// TestValidateTypoFailsFast Strict decoding rejects unknown fields naming them.
func TestValidateTypoFailsFast(t *testing.T) {
	bad := validBody + "bogus_field: true\n"
	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "bogus_field") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

// TestValidateMissingFile Missing files fail instead of yielding zero config.
func TestValidateMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestValidateLinearTeams linear.teams values must name configured
// repositories; a dangling mapping fails at load naming the team.
func TestValidateLinearTeams(t *testing.T) {
	good := validBody + `
linear:
  teams:
    ENG: acme/web
`
	if _, err := Load(writeConfig(t, good)); err != nil {
		t.Fatalf("mapped team must load: %v", err)
	}
	bad := validBody + `
linear:
  teams:
    ENG: ghost/repo
`
	_, err := Load(writeConfig(t, bad))
	if err == nil {
		t.Fatal("expected error for unmapped repository, got nil")
	}
	if !strings.Contains(err.Error(), "linear.teams") || !strings.Contains(err.Error(), "ghost/repo") {
		t.Errorf("error should name linear.teams and the bad value, got: %v", err)
	}
}

// A repository may name a local filesystem path instead of a GitHub
// remote: the path must be absolute, IsLocal flips, and Remote returns
// the path verbatim while non-local repos keep the https remote.
func TestRepositoryLocalPath(t *testing.T) {
	body := strings.Replace(validBody, "    enabled: true\n",
		"    enabled: true\n    local: /srv/git/acme-web.git\n", 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	repo := cfg.Repositories["acme/web"]
	if !repo.IsLocal() {
		t.Error("local path must mark the repository local")
	}
	if got := repo.Remote("acme/web"); got != "/srv/git/acme-web.git" {
		t.Errorf("Remote = %q, want the local path", got)
	}
	// A repo without local keeps the GitHub https remote.
	plain := RepositoryConfig{}
	if plain.IsLocal() {
		t.Error("empty local must not mark the repository local")
	}
	if got := plain.Remote("acme/web"); got != "https://github.com/acme/web.git" {
		t.Errorf("Remote = %q, want the github remote", got)
	}
}

// A relative local path fails at load: provisioning must never resolve
// it against whatever directory the daemon happened to start in.
func TestRepositoryLocalRelativeRejected(t *testing.T) {
	body := strings.Replace(validBody, "    enabled: true\n",
		"    enabled: true\n    local: ../repos/acme\n", 1)
	if _, err := Load(writeConfig(t, body)); err == nil ||
		!strings.Contains(err.Error(), "local") {
		t.Errorf("relative local path must fail validation, got %v", err)
	}
}
