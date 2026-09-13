// Command herder is the single self-contained Herder binary (issue #1):
// daemon, config validation, task operations, status, and diagnostics.
//
// Why: one binary, local-first; every later slice builds on these verbs.
// Approach: stdlib-only subcommand dispatch; --config/-c accepted before
// or after the subcommand; mutations hit the SQLite file directly while
// the daemon serves the read-only status view over the same file.
// Inputs: argv, $HERDER_CONFIG, ~/.config/herder/config.yaml.
// Flow: scan flags -> dispatch -> load config/open store as needed.
// Exit codes: 0 ok, 1 runtime failure, 2 usage or invalid config.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tmih06/herder/internal/api"
	"github.com/tmih06/herder/internal/config"
	"github.com/tmih06/herder/internal/health"
	"github.com/tmih06/herder/internal/ingest"
	"github.com/tmih06/herder/internal/storage"
	"github.com/tmih06/herder/internal/tasks"
)

// Version is the binary version for `herder version`.
const Version = "v0.1.0"

// main is the herder entry point. Purpose: exit-code discipline in one
// place. Approach: run() does everything and returns the code; main only
// passes stdio through. Returns: process exit code via os.Exit.
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches argv and returns the process exit code (testable seam:
// no os.Exit inside, all output to w/ew).
func run(argv []string, w, ew io.Writer) int {
	configPath, rest := scanGlobalFlags(argv)
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		printUsage(w)
		return 0
	}
	cmd, args := rest[0], rest[1:]
	switch cmd {
	case "version":
		fmt.Fprintln(w, "herder", Version)
		return 0
	case "init":
		return cmdInit(resolveConfigPath(configPath), args, w, ew)
	case "daemon":
		return cmdDaemon(resolveConfigPath(configPath), w, ew)
	case "config":
		return cmdConfig(resolveConfigPath(configPath), args, w, ew)
	case "status":
		return cmdStatus(resolveConfigPath(configPath), w, ew)
	case "doctor", "health":
		return cmdDoctor(resolveConfigPath(configPath), w, ew)
	case "task":
		return cmdTask(resolveConfigPath(configPath), args, w, ew)
	case "sandbox":
		return cmdSandbox(resolveConfigPath(configPath), args, w, ew)
	case "ingest":
		return cmdIngest(resolveConfigPath(configPath), args, w, ew)
	default:
		fmt.Fprintf(ew, "herder: unknown command %q\n\n", cmd)
		printUsage(ew)
		return 2
	}
}

// scanGlobalFlags extracts --config/-c wherever it appears (before or
// after the subcommand) and returns its value plus the remaining argv.
// A -- separator ends flag scanning so sandboxed commands (e.g. sh -c)
// pass through untouched.
func scanGlobalFlags(argv []string) (string, []string) {
	var configPath string
	rest := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--":
			rest = append(rest, argv[i:]...)
			return configPath, rest
		case strings.HasPrefix(a, "--config="):
			configPath = strings.TrimPrefix(a, "--config=")
		case (a == "--config" || a == "-c") && i+1 < len(argv):
			i++
			configPath = argv[i]
		default:
			rest = append(rest, a)
		}
	}
	return configPath, rest
}

// resolveConfigPath applies flag > $HERDER_CONFIG > default location.
func resolveConfigPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("HERDER_CONFIG"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "herder.yaml"
	}
	return filepath.Join(home, ".config", "herder", "config.yaml")
}

// loadConfig loads and validates, printing the actionable error itself.
// Returns nil on failure so callers just return the exit code.
func loadConfig(path string, ew io.Writer) *config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return nil
	}
	return cfg
}

// openStore opens the validated config's state file.
func openStore(cfg *config.Config, ew io.Writer) *storage.Store {
	store, err := storage.Open(cfg.Database.Path)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return nil
	}
	return store
}

// cmdInit writes the example config to path (the "small config" from the
// issue). Refuses to overwrite unless --force is given.
func cmdInit(path string, args []string, w, ew io.Writer) int {
	force := false
	for _, a := range args {
		if a == "--force" {
			force = true
		} else {
			fmt.Fprintf(ew, "herder: init takes only --force, got %q\n", a)
			return 2
		}
	}
	if _, err := os.Stat(path); err == nil && !force {
		fmt.Fprintf(ew, "herder: %s exists (use --force to overwrite)\n", path)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		fmt.Fprintf(ew, "herder: create config dir: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, []byte(config.ExampleYAML), 0o600); err != nil {
		fmt.Fprintf(ew, "herder: write config: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "herder: wrote example config to %s\n", path)
	return 0
}

// cmdDaemon validates config, initializes state, and serves the status
// view until SIGINT/SIGTERM. Config errors fail fast with exit 2.
func cmdDaemon(path string, w, ew io.Writer) int {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 2
	}
	store, err := storage.Open(cfg.Database.Path)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	defer store.Close()
	initial, err := store.ListTasks()
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "herder: state %s (%d tasks)\n", store.Path(), len(initial))
	fmt.Fprintf(w, "herder: listening on http://%s\n", cfg.Server.Listen)
	srv := api.New(cfg, store, path)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	select {
	case err := <-errCh:
		if err != nil && !strings.Contains(err.Error(), "Server closed") {
			fmt.Fprintf(ew, "herder: serve: %v\n", err)
			return 1
		}
		return 0
	case sig := <-sigCh:
		fmt.Fprintf(w, "herder: received %s, shutting down\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			fmt.Fprintf(ew, "herder: shutdown: %v\n", err)
			return 1
		}
		return 0
	}
}

// cmdConfig implements `herder config validate`: fail fast with the
// field-level message, or confirm the file is usable.
func cmdConfig(path string, args []string, w, ew io.Writer) int {
	if len(args) != 1 || args[0] != "validate" {
		fmt.Fprintf(ew, "herder: usage: herder config validate\n")
		return 2
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 2
	}
	fmt.Fprintf(w, "herder: config %s valid (%d repositories, %d agents)\n",
		path, len(cfg.Repositories), len(cfg.Agents))
	return 0
}

// cmdStatus queries the running daemon's task list over HTTP.
func cmdStatus(path string, w, ew io.Writer) int {
	cfg := loadConfig(path, ew)
	if cfg == nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.Server.Listen+"/v1/tasks", nil)
	if err != nil {
		fmt.Fprintf(ew, "herder: build status request: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(ew, "herder: cannot reach daemon at http://%s (%v)\n", cfg.Server.Listen, err)
		fmt.Fprintf(ew, "herder: is `herder daemon --config %s` running?\n", path)
		return 1
	}
	defer resp.Body.Close()
	var payload struct {
		Tasks []tasks.Task `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		fmt.Fprintf(ew, "herder: decode daemon response: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "%d tasks\n", len(payload.Tasks))
	for _, t := range payload.Tasks {
		fmt.Fprintf(w, "- %s %s %s %s %s\n", t.ID, t.Status, t.SourceRef, t.Repository, t.AgentProfile)
	}
	return 0
}

// cmdDoctor prints the four diagnostic sections distinctly and fails only
// when the must-work layers (controller, storage) fail; Herdr/Docker
// unreachable are setup hints, not fatal.
func cmdDoctor(path string, w, ew io.Writer) int {
	cfg, cfgErr := config.Load(path)
	var store *storage.Store
	if cfgErr == nil {
		var openErr error
		store, openErr = storage.Open(cfg.Database.Path)
		if openErr != nil {
			cfgErr = openErr
			_ = store
			store = nil
		} else {
			defer store.Close()
		}
	}
	report := health.Build(cfg, path, cfgErr, store)
	for _, section := range []health.Check{report.Controller, report.Storage, report.Herdr, report.Docker} {
		fmt.Fprintf(w, "%-10s %s — %s\n", section.Name+":", section.State, section.Detail)
	}
	if !report.OK() {
		return 1
	}
	return 0
}

// cmdTask implements `herder task list|inspect|create|transition|event`
// against the SQLite file directly.
func cmdTask(path string, args []string, w, ew io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(ew, "herder: usage: herder task list|inspect|create|transition|event ...\n")
		return 2
	}
	cfg := loadConfig(path, ew)
	if cfg == nil {
		return 2
	}
	store := openStore(cfg, ew)
	if store == nil {
		return 1
	}
	defer store.Close()
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return taskList(store, w, ew)
	case "inspect":
		return taskInspect(store, rest, w, ew)
	case "create":
		return taskCreate(cfg, store, rest, w, ew)
	case "transition":
		return taskTransition(store, rest, w, ew)
	case "event":
		return taskEvent(store, rest, w, ew)
	default:
		fmt.Fprintf(ew, "herder: unknown task subcommand %q\n", sub)
		return 2
	}
}

// taskList prints every task oldest-first.
func taskList(store *storage.Store, w, ew io.Writer) int {
	found, err := store.ListTasks()
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "%d tasks\n", len(found))
	for _, t := range found {
		fmt.Fprintf(w, "- %s %s %s %s %s\n", t.ID, t.Status, t.SourceRef, t.Repository, t.AgentProfile)
	}
	return 0
}

// taskInspect prints one task plus its full event history.
func taskInspect(store *storage.Store, args []string, w, ew io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(ew, "herder: usage: herder task inspect <id>\n")
		return 2
	}
	task, err := store.GetTask(args[0])
	if err != nil {
		fmt.Fprintf(ew, "herder: task %q not found\n", args[0])
		return 1
	}
	fmt.Fprintf(w, "id: %s\nstatus: %s\nsource: %s:%s\nrepository: %s\nagent: %s\nbranch: %s\nattempt: %d\ncreated: %s\nupdated: %s\n",
		task.ID, task.Status, task.SourceProvider, task.SourceRef,
		task.Repository, task.AgentProfile, task.BranchName, task.Attempt,
		task.CreatedAt.Format(time.RFC3339), task.UpdatedAt.Format(time.RFC3339))
	events, err := store.ListEvents(task.ID)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "events (%d):\n", len(events))
	for _, e := range events {
		fmt.Fprintf(w, "- %s %s %s/%s %s\n", e.CreatedAt.Format(time.RFC3339),
			e.Type, e.ActorType, e.ActorID, e.Payload)
	}
	return 0
}

// taskCreate opens a task for a configured repository, defaulting the
// agent profile to the repo's configured default.
func taskCreate(cfg *config.Config, store *storage.Store, args []string, w, ew io.Writer) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(ew)
	repo := fs.String("repo", "", "repository name as in config (required)")
	sourceRef := fs.String("source-ref", "", "issue reference, e.g. acme/web#7 (required)")
	provider := fs.String("source-provider", "github", "work provider name")
	agent := fs.String("agent", "", "agent profile (default: repo default)")
	branch := fs.String("branch", "", "working branch name")
	actorType := fs.String("actor-type", "controller", "event actor type")
	actorID := fs.String("actor-id", "cli", "event actor id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *repo == "" || *sourceRef == "" {
		fmt.Fprintf(ew, "herder: usage: herder task create --repo R --source-ref REF [--agent P] [--branch B]\n")
		return 2
	}
	repoCfg, ok := cfg.Repositories[*repo]
	if !ok {
		fmt.Fprintf(ew, "herder: repository %q not in config (defined: %s)\n",
			*repo, strings.Join(config.RepositoryNames(cfg), ", "))
		return 1
	}
	profile := *agent
	if profile == "" {
		profile = repoCfg.Agent.Default
	}
	if _, ok := cfg.Agents[profile]; !ok {
		fmt.Fprintf(ew, "herder: agent profile %q unknown (defined: %s)\n",
			profile, strings.Join(config.AgentNames(cfg), ", "))
		return 1
	}
	created, err := store.CreateTask(storage.CreateInput{
		SourceProvider: *provider, SourceRef: *sourceRef,
		Repository: *repo, AgentProfile: profile, BranchName: *branch,
		ActorType: *actorType, ActorID: *actorID,
	})
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "%s\n", created.ID)
	return 0
}

// taskTransition moves a task to a new state, appending the event.
func taskTransition(store *storage.Store, args []string, w, ew io.Writer) int {
	fs := flag.NewFlagSet("transition", flag.ContinueOnError)
	fs.SetOutput(ew)
	actorType := fs.String("actor-type", "controller", "event actor type")
	actorID := fs.String("actor-id", "cli", "event actor id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintf(ew, "herder: usage: herder task transition [--actor-type T] [--actor-id I] <id> <STATE> (flags first)\n")
		return 2
	}
	event, err := store.Transition(fs.Arg(0), tasks.State(fs.Arg(1)), *actorType, *actorID)
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "herder: %s -> %s (%s)\n", fs.Arg(0), fs.Arg(1), event.CreatedAt.Format(time.RFC3339))
	return 0
}

// taskEvent appends a custom structured event to a task's history.
func taskEvent(store *storage.Store, args []string, w, ew io.Writer) int {
	fs := flag.NewFlagSet("event", flag.ContinueOnError)
	fs.SetOutput(ew)
	payload := fs.String("payload", "{}", "JSON payload")
	actorType := fs.String("actor-type", "controller", "event actor type")
	actorID := fs.String("actor-id", "cli", "event actor id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintf(ew, "herder: usage: herder task event [--payload '{}'] [--actor-type T] [--actor-id I] <id> <type> (flags first)\n")
		return 2
	}
	if _, err := store.AppendEvent(fs.Arg(0), fs.Arg(1), *actorType, *actorID, *payload); err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "herder: appended %s to %s\n", fs.Arg(1), fs.Arg(0))
	return 0
}

// labelList collects repeatable --label flags into the issue label set.
type labelList []string

// String renders the collected labels for flag usage output.
func (l *labelList) String() string { return strings.Join(*l, ",") }

// Set appends one --label occurrence; repeat the flag for several labels.
func (l *labelList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// cmdIngest simulates one webhook delivery through the same policy gate
// the daemon's webhook endpoint uses, or lists recorded deliveries.
// Purpose: operators replay and inspect trigger decisions without
// crafting GitHub payloads by hand.
func cmdIngest(path string, args []string, w, ew io.Writer) int {
	if len(args) > 0 && args[0] == "log" {
		return ingestLog(path, args[1:], w, ew)
	}
	return ingestDelivery(path, args, w, ew)
}

// ingestDelivery claims one issue for one delivery id and prints the
// durable decision: accepted with its task, duplicate pointing at the
// survivor, or policy_denied with the reason and no task.
func ingestDelivery(path string, args []string, w, ew io.Writer) int {
	cfg := loadConfig(path, ew)
	if cfg == nil {
		return 2
	}
	store := openStore(cfg, ew)
	if store == nil {
		return 1
	}
	defer store.Close()
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(ew)
	delivery := fs.String("delivery", "", "webhook delivery id (required)")
	repo := fs.String("repo", "", "repository name as in config (required)")
	issue := fs.Int("issue", 0, "issue number (required)")
	title := fs.String("title", "", "issue title for branch naming")
	var labels labelList
	fs.Var(&labels, "label", "issue label (repeat for several)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *delivery == "" || *repo == "" || *issue < 1 {
		fmt.Fprintf(ew, "herder: usage: herder ingest --delivery ID --repo R --issue N [--title T] [--label L]...\n")
		return 2
	}
	out, err := ingest.New(cfg, store).Handle(ingest.IssueEvent{
		DeliveryID: *delivery, Repository: *repo,
		IssueNumber: *issue, Title: *title, Labels: labels,
	})
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "decision: %s\n", out.Decision)
	if out.TaskID != "" {
		fmt.Fprintf(w, "task: %s\n", out.TaskID)
	}
	fmt.Fprintf(w, "reason: %s\n", out.Reason)
	return 0
}

// ingestLog lists recorded deliveries oldest-first: accepted tasks and
// the policy-denied or duplicate non-events.
func ingestLog(path string, args []string, w, ew io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(ew, "herder: usage: herder ingest log\n")
		return 2
	}
	cfg := loadConfig(path, ew)
	if cfg == nil {
		return 2
	}
	store := openStore(cfg, ew)
	if store == nil {
		return 1
	}
	defer store.Close()
	found, err := store.ListDeliveries()
	if err != nil {
		fmt.Fprintf(ew, "herder: %v\n", err)
		return 1
	}
	fmt.Fprintf(w, "%d deliveries\n", len(found))
	for _, d := range found {
		task := d.TaskID
		if task == "" {
			task = "-"
		}
		fmt.Fprintf(w, "- %s %s %s %s %s %s\n",
			d.DeliveryID, d.Decision, d.SourceRef, d.Repository, task, d.Reason)
	}
	return 0
}

// printUsage lists the v0.1 command surface.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `herder — control plane for autonomous coding-agent fleets

usage: herder [--config PATH] <command> [args]

  init                    write example config (use --force to overwrite)
  daemon                  start the controller and serve the status view
  config validate         fail fast on a bad config with a field-level message
  status                  show the daemon's task list
  doctor | health         report controller, storage, Herdr, Docker distinctly
  task list               show tasks oldest-first
  task inspect <id>       show one task plus its event history
  task create --repo R --source-ref REF
                          open a task for a configured repository
  task transition [--actor-type T] [--actor-id I] <id> <STATE>
                          move a task, appending a structured event
  sandbox provision <task-id>
                          create or reuse the task's isolated container
  sandbox exec <id> -- <command...>
                          run a command inside the sandbox
  sandbox list|inspect|shell|stop|destroy
                          manage live sandboxes (id = task or container)
  ingest --delivery ID --repo R --issue N [--label L]...
                          gate and claim one delivery, printing the decision
  ingest log            show recorded deliveries and their decisions
  version                 print the binary version
  help                    print this text
`)
}
