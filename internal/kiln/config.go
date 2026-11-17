package kiln

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultFile = "kiln.yaml"

type Config struct {
	Name      string            `yaml:"name" json:"name"`
	Backend   string            `yaml:"backend" json:"backend"`
	Image     string            `yaml:"image" json:"image"`
	Command   []string          `yaml:"command,omitempty" json:"command,omitempty"`
	Env       map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Secrets   []string          `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Inputs    []string          `yaml:"inputs,omitempty" json:"inputs,omitempty"` // files/dirs copied to /kiln/inputs
	Budget    Budget            `yaml:"budget" json:"budget"`
	Resources Resources         `yaml:"resources" json:"resources"`
	AWS       *AWSConfig        `yaml:"aws,omitempty" json:"aws,omitempty"`
}

// AWSConfig picks which kiln stack (from deploy/aws) to run on.
type AWSConfig struct {
	Region string `yaml:"region" json:"region"`
	Stack  string `yaml:"stack" json:"stack"`
}

type Budget struct {
	MaxUSD       float64  `yaml:"max_usd" json:"max_usd"`
	MaxHours     float64  `yaml:"max_hours" json:"max_hours"`
	WarnAt       float64  `yaml:"warn_at" json:"warn_at"`
	PricePerHour *float64 `yaml:"price_per_hour,omitempty" json:"price_per_hour,omitempty"`
}

type Resources struct {
	CPU      float64 `yaml:"cpu" json:"cpu"`
	MemoryGB float64 `yaml:"memory_gb" json:"memory_gb"`
	Spot     bool    `yaml:"spot,omitempty" json:"spot,omitempty"`
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	envRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// LoadConfig reads, defaults and validates a kiln.yaml.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, &Error{Code: "config_not_found", Message: path + " not found", Fix: "run `kiln init` to create one"}
	}
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	var c Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		msg := err.Error()
		if errors.Is(err, io.EOF) {
			msg = "file is empty"
		}
		return Config{}, &Error{Code: "invalid_config", Message: path + ": " + msg, Fix: "check field names against SPEC.md"}
	}
	c.applyDefaults()
	return c, c.Validate()
}

// applyDefaults fills zero values. A cap can never be turned off: 0 means "use the default".
func (c *Config) applyDefaults() {
	if c.Backend == "" {
		c.Backend = "docker"
	}
	if c.Budget.MaxUSD == 0 {
		c.Budget.MaxUSD = 5
	}
	if c.Budget.MaxHours == 0 {
		c.Budget.MaxHours = 1
	}
	if c.Budget.WarnAt == 0 {
		c.Budget.WarnAt = 0.8
	}
	if c.Resources.CPU == 0 {
		c.Resources.CPU = 1
	}
	if c.Resources.MemoryGB == 0 {
		c.Resources.MemoryGB = 2
	}
	if c.Backend == "aws" {
		if c.AWS == nil {
			c.AWS = &AWSConfig{}
		}
		if c.AWS.Region == "" {
			c.AWS.Region = "us-east-1"
		}
		if c.AWS.Stack == "" {
			c.AWS.Stack = "kiln"
		}
	}
}

func (c Config) Validate() error {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }

	if !nameRe.MatchString(c.Name) {
		add("name: use lowercase letters, digits and dashes, max 63 chars (got %q)", c.Name)
	}
	if strings.TrimSpace(c.Image) == "" {
		add("image: required")
	}
	if _, ok := backends[c.Backend]; !ok {
		add("backend: unknown %q (supported: %s)", c.Backend, strings.Join(slices.Sorted(maps.Keys(backends)), ", "))
	}
	b := c.Budget
	if b.MaxUSD < 0 {
		add("budget.max_usd: must be positive")
	}
	if b.MaxHours < 0 {
		add("budget.max_hours: must be positive")
	}
	if b.WarnAt < 0 || b.WarnAt > 1 {
		add("budget.warn_at: must be between 0 and 1 (got %g)", b.WarnAt)
	}
	if b.PricePerHour != nil && *b.PricePerHour < 0 {
		add("budget.price_per_hour: must not be negative")
	}
	if c.Resources.CPU < 0 {
		add("resources.cpu: must be positive")
	}
	if c.Resources.MemoryGB < 0 {
		add("resources.memory_gb: must be positive")
	}
	if c.AWS != nil && c.Backend != "aws" {
		add("aws: only used with backend: aws")
	}
	if c.AWS != nil && !nameRe.MatchString(c.AWS.Stack) {
		add("aws.stack: use lowercase letters, digits and dashes (got %q)", c.AWS.Stack)
	}
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		if !envRe.MatchString(k) {
			add("env: invalid variable name %q", k)
		}
	}
	for _, s := range c.Secrets {
		if !envRe.MatchString(s) {
			add("secrets: invalid variable name %q", s)
		}
		if _, dup := c.Env[s]; dup {
			add("secrets: %q is also set in env; secret values must not be stored in config", s)
		}
	}
	if len(p) > 0 {
		return &Error{Code: "invalid_config", Message: strings.Join(p, "; "), Fix: "edit kiln.yaml, then run `kiln validate`"}
	}
	return nil
}

// CheckSecrets fails if a declared secret is not set in the environment.
func (c Config) CheckSecrets(lookup func(string) (string, bool)) error {
	var missing []string
	for _, s := range c.Secrets {
		if v, ok := lookup(s); !ok || v == "" {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return &Error{Code: "missing_secret", Message: "not set in environment: " + strings.Join(missing, ", "),
			Fix: "export " + missing[0] + "=... before running"}
	}
	return nil
}

// PinImage replaces image's tag with digest ("repo:tag" -> "repo@sha256:..."), so a rerun
// uses exactly the image an earlier run did even if the tag has since moved.
func PinImage(image, digest string) string {
	if digest == "" || strings.Contains(image, "@") {
		return image
	}
	repo := image
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		repo = image[:i] // a colon after the last slash is a tag; before it, a registry port
	}
	return repo + "@" + digest
}
