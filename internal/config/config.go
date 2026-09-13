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

// GithubConfig holds GitHub App credentials (env-expanded).
type GithubConfig struct {
	AppID          string `yaml:"app_id"`
	PrivateKeyFile string `yaml:"private_key_file"`
}

// SchedulerConfig bounds concurrent workers.
type SchedulerConfig struct {
	MaxWorkers int `yaml:"max_workers"`
}

// RepositoryConfig is the per-repo policy: trigger, agent, sandbox,
// validation, and delivery.
type RepositoryConfig struct {
	Enabled    bool             `yaml:"enabled"`
	Trigger    TriggerConfig    `yaml:"trigger"`
	Agent      RepoAgentConfig  `yaml:"agent"`
	Sandbox    SandboxConfig    `yaml:"sandbox"`
	Validation ValidationConfig `yaml:"validation"`
	Delivery   DeliveryConfig   `yaml:"delivery"`
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

// ValidationConfig lists commands that must pass before delivery.
type ValidationConfig struct {
	Commands []string `yaml:"commands"`
}

// DeliveryConfig controls pull-request delivery.
type DeliveryConfig struct {
	CreatePR  bool `yaml:"create_pr"`
	AutoMerge bool `yaml:"auto_merge"`
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
	return errs
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
