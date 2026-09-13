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

// TestIngestAcceptsThroughCLI An eligible delivery claims a queued task
// visible in task list, task inspect, and ingest log end to end.
func TestIngestAcceptsThroughCLI(t *testing.T) {
	cfg := writeTestConfig(t)
	code, out, _ := runCmd(t, "--config", cfg, "ingest",
		"--delivery", "del-1", "--repo", "acme/web", "--issue", "7",
		"--title", "Fix refresh race", "--label", "bug", "--label", "agent-ready")
	if code != 0 {
		t.Fatalf("ingest = %d, want 0: %s", code, out)
	}
	if !strings.Contains(out, "decision: accepted") || !strings.Contains(out, "task: task_") {
		t.Fatalf("ingest must print accepted with a task id, got:\n%s", out)
	}
	if _, out, _ := runCmd(t, "--config", cfg, "task", "list"); !strings.Contains(out, "1 tasks") {
		t.Errorf("task list must show the claimed task, got:\n%s", out)
	}
	taskID := strings.TrimSpace(strings.Split(strings.Split(out, "task: ")[1], "\n")[0])
	if _, out, _ := runCmd(t, "--config", cfg, "task", "inspect", taskID); !strings.Contains(out, "policy.decision") {
		t.Errorf("task inspect must show the policy outcome, got:\n%s", out)
	}
	if _, out, _ := runCmd(t, "--config", cfg, "ingest", "log"); !strings.Contains(out, "del-1 accepted") {
		t.Errorf("ingest log must record the accepted delivery, got:\n%s", out)
	}
}

// TestIngestDuplicatesThroughCLI Redeliveries converge without a second task.
func TestIngestDuplicatesThroughCLI(t *testing.T) {
	cfg := writeTestConfig(t)
	flags := []string{
		"--config", cfg, "ingest",
		"--delivery", "del-1", "--repo", "acme/web", "--issue", "7", "--label", "agent-ready",
	}
	if code, _, _ := runCmd(t, flags...); code != 0 {
		t.Fatalf("first ingest = %d, want 0", code)
	}
	code, out, _ := runCmd(t, flags...)
	if code != 0 || !strings.Contains(out, "decision: duplicate") {
		t.Fatalf("redelivery must report duplicate, got %d:\n%s", code, out)
	}
	if _, out, _ := runCmd(t, "--config", cfg, "task", "list"); !strings.Contains(out, "1 tasks") {
		t.Errorf("redelivery must not create a second task, got:\n%s", out)
	}
}

// TestIngestDeniedThroughCLI Ineligible deliveries deny with a reason,
// create no task, and stay visible in ingest log.
func TestIngestDeniedThroughCLI(t *testing.T) {
	cfg := writeTestConfig(t)
	code, out, _ := runCmd(t, "--config", cfg, "ingest",
		"--delivery", "del-evil", "--repo", "evil/repo", "--issue", "1", "--label", "agent-ready")
	if code != 0 || !strings.Contains(out, "decision: policy_denied") {
		t.Fatalf("unknown repo must deny, got %d:\n%s", code, out)
	}
	if strings.Contains(out, "task: ") {
		t.Errorf("denial must not print a task, got:\n%s", out)
	}
	if _, out, _ := runCmd(t, "--config", cfg, "task", "list"); !strings.Contains(out, "0 tasks") {
		t.Errorf("denial must create no task, got:\n%s", out)
	}
	if _, out, _ := runCmd(t, "--config", cfg, "ingest", "log"); !strings.Contains(out, "del-evil policy_denied") {
		t.Errorf("ingest log must record the denial, got:\n%s", out)
	}
	code, _, _ = runCmd(t, "--config", cfg, "ingest", "--delivery", "d", "--repo", "acme/web")
	if code != 2 {
		t.Errorf("ingest without --issue = %d, want usage exit 2", code)
	}
}

// TestFlagScanStopsAtSeparator commands after -- (e.g. sh -c inside
// sandbox exec) must pass through untouched, not parsed as --config.
func TestFlagScanStopsAtSeparator(t *testing.T) {
	cfg, rest := scanGlobalFlags([]string{"--config", "herder.yaml", "sandbox", "exec", "task_1", "--", "sh", "-c", "echo hi"})
	if cfg != "herder.yaml" {
		t.Errorf("config = %q, want herder.yaml", cfg)
	}
	want := []string{"sandbox", "exec", "task_1", "--", "sh", "-c", "echo hi"}
	if strings.Join(rest, " ") != strings.Join(want, " ") {
		t.Errorf("rest = %v, want %v", rest, want)
	}
}
