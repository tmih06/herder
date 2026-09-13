package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmih06/herder/internal/config"
)

// runCmd runs the CLI with argv and returns exit code plus outputs.
func runCmd(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(argv, &out, &errOut)
	return code, out.String(), errOut.String()
}

// writeTestConfig writes a minimal valid config with a temp database.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := strings.ReplaceAll(config.ExampleYAML,
		"~/.local/state/herder/herder.db", filepath.Join(dir, "herder.db"))
	body = strings.ReplaceAll(body, "tmih06/meltiply", "acme/web")
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestUnknownCommand Unknown verbs exit 2 with usage.
func TestUnknownCommand(t *testing.T) {
	code, _, errOut := runCmd(t, "frobnicate")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Errorf("stderr should name the problem, got %q", errOut)
	}
}

// TestInitWritesValidExample init writes a config that validates; second init refuses without --force.
func TestInitWritesValidExample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if code, _, _ := runCmd(t, "--config", path, "init"); code != 0 {
		t.Fatalf("init exit = %d, want 0", code)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("init output must validate: %v", err)
	}
	if len(cfg.Repositories) != 1 {
		t.Errorf("init output should carry one repository, got %d", len(cfg.Repositories))
	}
	if code, _, _ := runCmd(t, "--config", path, "init"); code != 1 {
		t.Errorf("second init exit = %d, want 1 (refuse overwrite)", code)
	}
	if code, _, _ := runCmd(t, "--config", path, "init", "--force"); code != 0 {
		t.Errorf("init --force exit = %d, want 0", code)
	}
}

// TestConfigValidateRejectsBadTrigger config validate exits 2 naming trigger/labels.
func TestConfigValidateRejectsBadTrigger(t *testing.T) {
	path := writeTestConfig(t)
	raw, _ := os.ReadFile(path)
	bad := strings.Replace(string(raw), "labels:\n        - agent-ready", "labels: []", 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCmd(t, "config", "validate", "--config", path)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "trigger") && !strings.Contains(errOut, "label") {
		t.Errorf("stderr should name trigger/labels, got %q", errOut)
	}
}

// TestTaskLifecycleThroughCLI End-to-end CLI seam: create, list, transition, illegal jump, inspect, custom event.
func TestTaskLifecycleThroughCLI(t *testing.T) {
	path := writeTestConfig(t)
	withConfig := func(argv ...string) []string {
		return append([]string{"--config", path}, argv...)
	}

	code, id, _ := runCmd(t, withConfig("task", "create", "--repo", "acme/web", "--source-ref", "acme/web#7")...)
	if code != 0 {
		t.Fatalf("create exit = %d, want 0", code)
	}
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "task_") {
		t.Fatalf("create should print the task id, got %q", id)
	}

	if code, out, _ := runCmd(t, withConfig("task", "list")...); code != 0 || !strings.Contains(out, id) {
		t.Errorf("list exit = %d, output should contain %s, got %q", code, id, out)
	}
	if code, _, errOut := runCmd(t, withConfig("task", "transition", id, "ELIGIBLE")...); code != 0 {
		t.Errorf("transition exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if code, _, _ := runCmd(t, withConfig("task", "transition", id, "RUNNING")...); code != 1 {
		t.Errorf("illegal jump exit = %d, want 1", code)
	}
	if code, out, _ := runCmd(t, withConfig("task", "inspect", id)...); code != 0 ||
		!strings.Contains(out, "ELIGIBLE") || !strings.Contains(out, "task.created") ||
		!strings.Contains(out, "task.transition") {
		t.Errorf("inspect exit = %d, should show status plus both events, got %q", code, out)
	}
	if code, _, _ := runCmd(t, withConfig("task", "event", "--payload", `{"pane":"w3:p2"}`, id, "agent.working")...); code != 0 {
		t.Errorf("event exit = %d, want 0", code)
	}
	if code, out, _ := runCmd(t, withConfig("task", "inspect", id)...); code != 0 ||
		!strings.Contains(out, "agent.working") {
		t.Errorf("inspect exit = %d, should show the custom event, got %q", code, out)
	}
}

// TestTaskCreateUnknownRepo create against an unconfigured repo fails naming the repo.
func TestTaskCreateUnknownRepo(t *testing.T) {
	path := writeTestConfig(t)
	code, _, errOut := runCmd(t, "--config", path, "task", "create",
		"--repo", "ghost/repo", "--source-ref", "x#1")
	if code == 0 {
		t.Error("create with unknown repo must fail")
	}
	if !strings.Contains(errOut, "ghost/repo") {
		t.Errorf("stderr should name the unknown repo, got %q", errOut)
	}
}

// TestDoctorReportsFourSections doctor prints all four sections distinctly on a good setup.
func TestDoctorReportsFourSections(t *testing.T) {
	path := writeTestConfig(t)
	code, out, _ := runCmd(t, "--config", path, "doctor")
	if code != 0 {
		t.Errorf("doctor exit = %d, want 0 on a good setup", code)
	}
	for _, section := range []string{"controller:", "storage:", "herdr:", "docker:"} {
		if !strings.Contains(out, section) {
			t.Errorf("doctor output should report %q distinctly, got:\n%s", section, out)
		}
	}
}
