// Package health probes the four things a new contributor must diagnose
// distinctly (issue #1): controller config, SQLite storage, Herdr
// reachability, and Docker reachability.
//
// Why: "it doesn't work" is unactionable; four labeled sections point at
// the broken layer. Approach: pure checks with short timeouts, each
// returning ok/reachable or a state plus a fix hint. Herdr/Docker are
// warnings, never fatal: the daemon runs without them.
// Inputs: loaded config, config path, storage handle.
// Flow: CheckController -> CheckStorage -> CheckHerdr -> CheckDocker.
// Returns: a Report the CLI and /v1/health render verbatim.
package health

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/storage"
)

// probeTimeout bounds each external reachability probe.
const probeTimeout = 5 * time.Second

// Check is one labeled section of the report.
type Check struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// Report holds the four diagnostic sections.
type Report struct {
	Controller Check `json:"controller"`
	Storage    Check `json:"storage"`
	Herdr      Check `json:"herdr"`
	Docker     Check `json:"docker"`
}

// OK reports whether the must-work layers pass.
// Purpose: doctor exit code. Herdr/Docker may warn without failing OK.
// Returns true only when controller and storage are both ok.
func (r Report) OK() bool {
	return r.Controller.State == "ok" && r.Storage.State == "ok"
}

// Build assembles the full report. A nil cfg means the config failed to
// load; reason carries the load error for the controller section.
func Build(cfg *config.Config, cfgPath string, reason error, store *storage.Store) Report {
	return Report{
		Controller: CheckController(cfg, cfgPath, reason),
		Storage:    CheckStorage(store),
		Herdr:      CheckHerdr(),
		Docker:     CheckDocker(),
	}
}

// CheckController verifies configuration loaded and validated.
// Inputs: the loaded config (nil on load failure), its path, load error.
func CheckController(cfg *config.Config, cfgPath string, reason error) Check {
	if reason != nil || cfg == nil {
		return Check{Name: "controller", State: "fail",
			Detail: fmt.Sprintf("config %s invalid: %v", cfgPath, reason)}
	}
	return Check{Name: "controller", State: "ok",
		Detail: fmt.Sprintf("config %s valid (%d repositories, %d agents)",
			cfgPath, len(cfg.Repositories), len(cfg.Agents))}
}

// CheckStorage verifies the SQLite state file answers.
func CheckStorage(store *storage.Store) Check {
	if store == nil {
		return Check{Name: "storage", State: "fail", Detail: "state store not open"}
	}
	if err := store.Ping(); err != nil {
		return Check{Name: "storage", State: "fail", Detail: err.Error()}
	}
	tasks, err := store.ListTasks()
	if err != nil {
		return Check{Name: "storage", State: "fail", Detail: err.Error()}
	}
	return Check{Name: "storage", State: "ok",
		Detail: fmt.Sprintf("sqlite %s reachable (%d tasks)", store.Path(), len(tasks))}
}

// probeResult is the outcome of running one external binary.
type probeResult struct {
	path   string
	output string
	err    error
}

// probeBinary locates name in PATH and runs it under a timeout.
// Purpose: one place for LookPath, timeout, and first-line extraction so
// Herdr and Docker probes cannot drift apart. Inputs: binary name plus
// args. Returns path, first output line, and err (non-nil when missing or
// when the run fails; output is still captured for diagnosis).
func probeBinary(name string, argv ...string) probeResult {
	path, err := exec.LookPath(name)
	if err != nil {
		return probeResult{err: err}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, runErr := exec.CommandContext(ctx, path, argv...).CombinedOutput()
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return probeResult{path: path, output: first, err: runErr}
}

// CheckHerdr probes the Herdr runtime via its CLI.
// Reachable means the binary runs; otherwise the detail tells the
// operator whether to install Herdr or start its server.
func CheckHerdr() Check {
	probe := probeBinary("herdr", "status")
	if probe.err != nil && probe.path == "" {
		return Check{Name: "herdr", State: "unreachable",
			Detail: "herdr binary not in PATH (install Herdr to supervise agents)"}
	}
	if probe.err != nil {
		return Check{Name: "herdr", State: "unreachable",
			Detail: fmt.Sprintf("herdr found at %s but not answering (%s); start it with `herdr`",
				probe.path, orMsg(probe.output, probe.err.Error()))}
	}
	return Check{Name: "herdr", State: "reachable",
		Detail: fmt.Sprintf("herdr at %s: %s", probe.path, orMsg(probe.output, "server answering"))}
}

// CheckDocker probes the Docker sandbox provider via `docker info`.
// Unreachable only warns: task state still works without a sandbox host.
func CheckDocker() Check {
	probe := probeBinary("docker", "info", "--format", "{{.ServerVersion}}")
	if probe.err != nil && probe.path == "" {
		return Check{Name: "docker", State: "unreachable",
			Detail: "docker binary not in PATH (install Docker to run sandboxes)"}
	}
	if probe.err != nil {
		return Check{Name: "docker", State: "unreachable",
			Detail: "docker found but daemon not answering (is dockerd running?)"}
	}
	return Check{Name: "docker", State: "reachable",
		Detail: fmt.Sprintf("docker at %s: server %s", probe.path, orMsg(probe.output, "daemon answering"))}
}

// orMsg returns s unless blank, for probe detail fallbacks.
func orMsg(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
