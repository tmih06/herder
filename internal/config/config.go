// Package config loads and validates the Herder daemon configuration.
//
// Why: the daemon must fail fast on a bad trigger, an unknown agent
// profile, or an unknown sandbox provider with a message that tells the
// operator exactly which field to fix (issue #1 acceptance criteria).
// Approach: strict YAML decoding (unknown fields rejected) over an
// env-expanded file, then a single Validate pass returning every problem
// at once so one run fixes the whole file.
// Inputs: path to a YAML file shaped like examples/herder.yaml.
// Flow: read file -> expand ${ENV} -> strict unmarshal -> defaults ->
// validate. Returns the config or a joined actionable error.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultListen is used when server.listen is omitted.
const DefaultListen = "127.0.0.1:8787"

// Supported sandbox providers for v0.1 (SPEC section 24: Docker only).
var supportedSandboxProviders = []string{"docker"}

// Supported agent kinds for v0.1 (SPEC section 6: Codex + Claude first,
// plus other Herdr-supported CLI agents).
var supportedAgentKinds = []string{"codex", "claude", "opencode", "gemini"}

// Supported Herdr driver modes (SPEC section 17: CLI first, socket for
// long-lived production orchestration).
var supportedHerdrModes = []string{"socket", "cli", "disabled"}

var labelPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-/]+$`)

// Config is the root of examples/herder.yaml.
type Config struct {
	Server       ServerConfig                `yaml:"server"`
	Database     DatabaseConfig              `yaml:"database"`
	Herdr        HerdrConfig                 `yaml:"herdr"`
	Github       GithubConfig                `yaml:"github"`
	Linear       LinearConfig                `yaml:"linear"`
	API          APIConfig                   `yaml:"api"`
	Scheduler    SchedulerConfig             `yaml:"scheduler"`
	Repositories map[string]RepositoryConfig `yaml:"repositories"`
	Agents       map[string]AgentConfig      `yaml:"agents"`
}

// ServerConfig controls the local daemon API and status view.
type ServerConfig struct {
	Listen string `yaml:"listen"`
}

// DatabaseConfig locates the SQLite state file.
type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// HerdrConfig selects how Herder talks to the Herdr runtime.
type HerdrConfig struct {
	Mode string `yaml:"mode"`
}

// GithubConfig holds GitHub App credentials (env-expanded) plus the
// webhook signing secret. WebhookSecret empty means deliveries are not
// signature-verified (documented dev mode); set it in production.
type GithubConfig struct {
	AppID          string `yaml:"app_id"`
	PrivateKeyFile string `yaml:"private_key_file"`
	WebhookSecret  string `yaml:"webhook_secret"`
}

// LinearConfig holds the Linear webhook signing secret (env-expanded)
// and the team-key -> repository routing table. WebhookSecret empty
// means deliveries are not signature-verified (dev mode); an issue whose
// team key is absent from Teams records a policy_denied delivery.
type LinearConfig struct {
	WebhookSecret string            `yaml:"webhook_secret"`
	Teams         map[string]string `yaml:"teams"`
}

// APIConfig holds the bearer secret (env-expanded) that authorizes
// POST /v1/tasks direct submissions. Secret empty disables the endpoint:
// the daemon never runs an unauthenticated queue.
type APIConfig struct {
	Secret string `yaml:"secret"`
}

// SchedulerConfig bounds concurrent workers and paces the dispatch loop
// (SPEC section 15): a global cap, optional per-repository and per-agent
// -kind caps, the dispatch lease TTL, and the poll interval.
type SchedulerConfig struct {
	// MaxWorkers caps workers fleet-wide.
	MaxWorkers int `yaml:"max_workers"`
	// PerRepository caps workers per repository name; absent means the
	// global cap alone applies.
	PerRepository map[string]int `yaml:"per_repository"`
	// PerAgent caps workers per agent kind (codex, claude, ...); absent
	// means the global cap alone applies.
	PerAgent map[string]int `yaml:"per_agent"`
	// LeaseTTL bounds one dispatch lease as a Go duration; an owner that
	// stops heartbeating loses the task back to the queue.
	LeaseTTL string `yaml:"lease_ttl"`
	// DispatchInterval spaces scheduler passes as a Go duration.
	DispatchInterval string `yaml:"dispatch_interval"`
}

// DefaultLeaseTTL applies when scheduler.lease_ttl is omitted: long
// enough for a slow provision between heartbeats, short enough that a
// dead dispatcher's task requeues promptly.
const DefaultLeaseTTL = 2 * time.Minute

// DefaultDispatchInterval applies when scheduler.dispatch_interval is
// omitted.
const DefaultDispatchInterval = 2 * time.Second

// durationOr parses raw as a positive Go duration, returning def on
// empty or invalid input. Purpose: one place owns the string->duration
// rule the validator already checked. Inputs: raw config string and
// fallback. Returns the parsed duration or def.
func durationOr(raw string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return def
}

// LeaseTTLDuration parses the configured lease TTL with its default.
// Returns DefaultLeaseTTL on empty or invalid input.
func (s SchedulerConfig) LeaseTTLDuration() time.Duration {
	return durationOr(s.LeaseTTL, DefaultLeaseTTL)
}

// DispatchIntervalDuration parses the configured dispatch interval with
// its default. Returns DefaultDispatchInterval on empty or invalid input.
func (s SchedulerConfig) DispatchIntervalDuration() time.Duration {
	return durationOr(s.DispatchInterval, DefaultDispatchInterval)
}

// TimeoutDuration parses an agent profile's timeout; zero means no limit.
// Purpose: the scheduler's time-limit enforcement needs the duration the
// validator already checked. Returns 0 on empty or invalid input.
func (a AgentConfig) TimeoutDuration() time.Duration {
	return durationOr(a.Timeout, 0)
}

// RepositoryConfig is the per-repo policy: trigger, agent, sandbox,
// validation, and delivery. Local names a filesystem path to clone from
// and push to instead of GitHub — the whole point of API-queued work is
// that a repository need not exist on any forge.
type RepositoryConfig struct {
	Enabled    bool             `yaml:"enabled"`
	Local      string           `yaml:"local"`
	Trigger    TriggerConfig    `yaml:"trigger"`
	Agent      RepoAgentConfig  `yaml:"agent"`
	Sandbox    SandboxConfig    `yaml:"sandbox"`
	Validation ValidationConfig `yaml:"validation"`
	Delivery   DeliveryConfig   `yaml:"delivery"`
}

// IsLocal reports whether the repository clones/pushes a filesystem path
// instead of a GitHub remote: no forge, no PR, no issue labels.
func (r RepositoryConfig) IsLocal() bool { return strings.TrimSpace(r.Local) != "" }

// Remote returns the git URL provisioning clones and delivery pushes:
// the local path when configured, else the GitHub https remote for name.
func (r RepositoryConfig) Remote(name string) string {
	if r.IsLocal() {
		return strings.TrimSpace(r.Local)
	}
	return fmt.Sprintf("https://github.com/%s.git", name)
}

// TriggerConfig lists the labels that mark work as agent-ready.
type TriggerConfig struct {
	Labels []string `yaml:"labels"`
}

// RepoAgentConfig selects the default agent profile by name.
type RepoAgentConfig struct {
	Default string `yaml:"default"`
}

// SandboxConfig selects the isolation provider and its worker image.
// Image is optional: empty means the provider default.
type SandboxConfig struct {
	Provider string `yaml:"provider"`
	Image    string `yaml:"image"`
}

// ValidationConfig is the per-repo gate that must pass before delivery
// (SPEC section 33): shell commands run inside the task sandbox, glob
// patterns for paths the agent must never touch, and the clean-tree rule
// that refuses to ship uncommitted or untracked work. ForbiddenChanges
// patterns are repo-relative globs where ** crosses directory separators;
// RequireCleanGit defaults to true when omitted.
type ValidationConfig struct {
	Commands         []string `yaml:"commands"`
	ForbiddenChanges []string `yaml:"forbidden_changes"`
	RequireCleanGit  *bool    `yaml:"require_clean_git"`
}

// CleanTreeRequired reports whether validation must reject a dirty tree.
// Purpose: the zero value (field omitted) means "required" — shipping
// uncommitted work is the unsafe default, so opting out must be explicit.
func (v ValidationConfig) CleanTreeRequired() bool {
	return v.RequireCleanGit == nil || *v.RequireCleanGit
}

// DeliveryLabels names the issue labels marking each stage of the
// running -> review -> completed path (SPEC section 35). Empty fields
// fall back to the spec defaults in LabelSet.
type DeliveryLabels struct {
	Running   string `yaml:"running"`
	Review    string `yaml:"review"`
	Completed string `yaml:"completed"`
}

// DeliveryConfig controls pull-request delivery.
type DeliveryConfig struct {
	CreatePR  bool           `yaml:"create_pr"`
	AutoMerge bool           `yaml:"auto_merge"`
	Labels    DeliveryLabels `yaml:"labels"`
}

// LabelSet returns the configured stage labels with spec defaults filled:
// agent-running while the agent works, agent-review once the PR is open,
// completed when the task is done.
func (d DeliveryConfig) LabelSet() DeliveryLabels {
	l := d.Labels
	if l.Running == "" {
		l.Running = "agent-running"
	}
	if l.Review == "" {
		l.Review = "agent-review"
	}
	if l.Completed == "" {
		l.Completed = "completed"
	}
	return l
}

// AgentConfig is a launch profile for one coding-agent kind.
type AgentConfig struct {
	Kind      string            `yaml:"kind"`
	Timeout   string            `yaml:"timeout"`
	Resources map[string]string `yaml:"resources"`
}

// Load reads path, expands ${ENV} references, strictly decodes YAML,
// applies defaults, and validates. It returns every problem found so a
// single run can fix the whole file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(os.ExpandEnv(string(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if errs := cfg.validate(); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return nil, fmt.Errorf("config: invalid %s:\n  - %s", path, strings.Join(msgs, "\n  - "))
	}
	return &cfg, nil
}

// applyDefaults fills omitted scalar settings so a minimal config works.
func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Herdr.Mode == "" {
		c.Herdr.Mode = "socket"
	}
	if c.Scheduler.MaxWorkers == 0 {
		c.Scheduler.MaxWorkers = 4
	}
}

// validate checks every field and returns all problems (nil when valid).
func (c *Config) validate() []error {
	var errs []error
	if host, port, err := net.SplitHostPort(c.Server.Listen); err != nil || host == "" || !validPort(port) {
		errs = append(errs, fmt.Errorf("server.listen %q must be host:port with a numeric port (example %q)",
			c.Server.Listen, DefaultListen))
	}
	if strings.TrimSpace(c.Database.Path) == "" {
		errs = append(errs, errors.New("database.path is required (example \"~/.local/state/herder/herder.db\")"))
	}
	if !contains(supportedHerdrModes, c.Herdr.Mode) {
		errs = append(errs, fmt.Errorf("herdr.mode %q unknown: want one of %s",
			c.Herdr.Mode, strings.Join(supportedHerdrModes, ", ")))
	}
	if c.Scheduler.MaxWorkers < 1 {
		errs = append(errs, fmt.Errorf("scheduler.max_workers must be >= 1, got %d", c.Scheduler.MaxWorkers))
	}
	for name, limit := range c.Scheduler.PerRepository {
		if _, ok := c.Repositories[name]; !ok {
			errs = append(errs, fmt.Errorf("scheduler.per_repository.%q is not a configured repository (defined: %s)",
				name, strings.Join(sortedKeys(c.Repositories), ", ")))
		}
		if limit < 1 {
			errs = append(errs, fmt.Errorf("scheduler.per_repository.%q must be >= 1, got %d", name, limit))
		}
	}
	for kind, limit := range c.Scheduler.PerAgent {
		if !contains(supportedAgentKinds, kind) {
			errs = append(errs, fmt.Errorf("scheduler.per_agent.%q is not a supported agent kind (want one of %s)",
				kind, strings.Join(supportedAgentKinds, ", ")))
		}
		if limit < 1 {
			errs = append(errs, fmt.Errorf("scheduler.per_agent.%q must be >= 1, got %d", kind, limit))
		}
	}
	// Slice, not map: errors must come out in declaration order.
	for _, d := range []struct {
		field string
		raw   string
	}{
		{"scheduler.lease_ttl", c.Scheduler.LeaseTTL},
		{"scheduler.dispatch_interval", c.Scheduler.DispatchInterval},
	} {
		if d.raw == "" {
			continue
		}
		if parsed, err := time.ParseDuration(d.raw); err != nil || parsed <= 0 {
			errs = append(errs, fmt.Errorf("%s %q must be a positive Go duration (example \"2m\")", d.field, d.raw))
		}
	}
	for team, repo := range c.Linear.Teams {
		if strings.TrimSpace(team) == "" {
			errs = append(errs, errors.New("linear.teams: team keys must be non-empty"))
		}
		if _, ok := c.Repositories[repo]; !ok {
			errs = append(errs, fmt.Errorf("linear.teams.%q %q is not a configured repository (defined: %s)",
				team, repo, strings.Join(sortedKeys(c.Repositories), ", ")))
		}
	}
	if len(c.Repositories) == 0 {
		errs = append(errs, errors.New("repositories must define at least one repository (example \"owner/repo\")"))
	}
	for name, repo := range c.Repositories {
		errs = append(errs, validateRepository(name, repo, c.Agents)...)
	}
	if len(c.Agents) == 0 {
		errs = append(errs, errors.New("agents must define at least one agent profile"))
	}
	for name, agent := range c.Agents {
		if !contains(supportedAgentKinds, agent.Kind) {
			errs = append(errs, fmt.Errorf("agents.%q.kind %q unknown: want one of %s",
				name, agent.Kind, strings.Join(supportedAgentKinds, ", ")))
		}
		if agent.Timeout != "" {
			if d, err := time.ParseDuration(agent.Timeout); err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("agents.%q.timeout %q must be a positive Go duration (example \"2h\")",
					name, agent.Timeout))
			}
		}
	}
	return errs
}

// validateRepository checks one repository entry against the agent table.
// Purpose: catch bad triggers, dangling agent references, and unknown
// sandbox providers at load time instead of at first webhook.
func validateRepository(name string, repo RepositoryConfig, agents map[string]AgentConfig) []error {
	prefix := fmt.Sprintf("repositories.%q", name)
	var errs []error
	if len(repo.Trigger.Labels) == 0 {
		errs = append(errs, fmt.Errorf("%s.trigger.labels must list at least one label (example [agent-ready])", prefix))
	}
	for _, label := range repo.Trigger.Labels {
		if label == "" || !labelPattern.MatchString(label) {
			errs = append(errs, fmt.Errorf("%s.trigger.labels: label %q must match %s",
				prefix, label, labelPattern.String()))
		}
	}
	if repo.Local != "" && !filepath.IsAbs(repo.Local) {
		errs = append(errs, fmt.Errorf("%s.local %q must be an absolute path", prefix, repo.Local))
	}
	if repo.Agent.Default == "" {
		errs = append(errs, fmt.Errorf("%s.agent.default must name an entry in agents", prefix))
	} else if _, ok := agents[repo.Agent.Default]; !ok {
		errs = append(errs, fmt.Errorf("%s.agent.default %q unknown: defined agent profiles are [%s]",
			prefix, repo.Agent.Default, strings.Join(sortedKeys(agents), ", ")))
	}
	if !contains(supportedSandboxProviders, repo.Sandbox.Provider) {
		errs = append(errs, fmt.Errorf("%s.sandbox.provider %q unknown: want one of %s",
			prefix, repo.Sandbox.Provider, strings.Join(supportedSandboxProviders, ", ")))
	}
	for _, pattern := range repo.Validation.ForbiddenChanges {
		if err := validForbiddenPattern(pattern); err != nil {
			errs = append(errs, fmt.Errorf("%s.validation.forbidden_changes: %w", prefix, err))
		}
	}
	for stage, label := range map[string]string{
		"running":   repo.Delivery.Labels.Running,
		"review":    repo.Delivery.Labels.Review,
		"completed": repo.Delivery.Labels.Completed,
	} {
		if label != "" && !labelPattern.MatchString(label) {
			errs = append(errs, fmt.Errorf("%s.delivery.labels.%s %q must match %s",
				prefix, stage, label, labelPattern.String()))
		}
	}
	return errs
}

// validForbiddenPattern rejects globs that cannot name a repo-relative
// path: empty, absolute, escaping the root, or carrying whitespace or
// character classes (the matcher supports only *, ?, and **).
func validForbiddenPattern(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return errors.New("pattern must not be empty")
	}
	if strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "~") {
		return fmt.Errorf("pattern %q must be repo-relative, not absolute", pattern)
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == ".." {
			return fmt.Errorf("pattern %q must not escape the repository root", pattern)
		}
	}
	if strings.ContainsAny(pattern, " \t[]") {
		return fmt.Errorf("pattern %q supports only *, ?, and ** (no spaces or character classes)", pattern)
	}
	return nil
}

// validPort reports whether s is a TCP port number.
// Purpose: guard server.listen against typos like "localhost:http".
// Inputs: the port substring after the colon. Returns true for 1-65535.
func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// contains reports whether list holds s.
// Purpose: membership tests for provider/kind/mode allow-lists.
// Inputs: allow-list and candidate. Returns true on exact match.
func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// sortedKeys returns the map keys in sorted order for stable messages.
// Purpose: one helper for every name list in errors and CLI output.
// Inputs: any map with string keys. Returns sorted keys, never nil.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RepositoryNames returns the sorted repository names for CLI messages.
// Purpose: actionable errors name the valid choices. Inputs: loaded config.
// Returns: sorted names, never nil.
func RepositoryNames(c *Config) []string { return sortedKeys(c.Repositories) }

// AgentNames returns the sorted agent profile names for CLI messages.
func AgentNames(c *Config) []string { return sortedKeys(c.Agents) }
