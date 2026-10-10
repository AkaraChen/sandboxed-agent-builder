// Package config is the builder's configuration file and the small
// adapters that come straight from it (build secrets).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/registry"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen   string `yaml:"listen"`
	DataDir  string `yaml:"dataDir"`
	Database string `yaml:"database"`
	Docker   struct {
		Host string `yaml:"host"`
	} `yaml:"docker"`
	Worker struct {
		ID           string        `yaml:"id"`
		Concurrency  int           `yaml:"concurrency"`
		PollInterval time.Duration `yaml:"pollInterval"`
		Lease        time.Duration `yaml:"lease"`
	} `yaml:"worker"`
	Logs struct {
		MaxBytesPerAttempt int64 `yaml:"maxBytesPerAttempt"`
	} `yaml:"logs"`
	Auth struct {
		Callers []struct {
			ID          string `yaml:"id"`
			TokenSHA256 string `yaml:"tokenSha256"`
		} `yaml:"callers"`
		// AnonymousCaller serves requests without a token; local
		// development only.
		AnonymousCaller string `yaml:"anonymousCaller"`
	} `yaml:"auth"`
	// BuildSecrets are values build scripts may mount by id; each comes
	// from an environment variable or a file and is never stored.
	BuildSecrets map[string]struct {
		Env  string `yaml:"env"`
		File string `yaml:"file"`
	} `yaml:"buildSecrets"`
	Plugins struct {
		AllowFileSources bool `yaml:"allowFileSources"`
	} `yaml:"plugins"`
	// AlpineVerified opens the alpine base family; see docs/verification.md.
	AlpineVerified bool `yaml:"alpineVerified"`
	Upstreams      struct {
		NPMRegistry      string `yaml:"npmRegistry"`
		NodeDist         string `yaml:"nodeDist"`
		NodeMuslDist     string `yaml:"nodeMuslDist"`
		AkhookRepository string `yaml:"akhookRepository"`
		AkhookRef        string `yaml:"akhookRef"`
		ToolchainImage   string `yaml:"toolchainImage"`
	} `yaml:"upstreams"`
	Registry struct {
		Enabled         bool `yaml:"enabled"`
		registry.Config `yaml:",inline"`
	} `yaml:"registry"`
}

// Load reads path ("" for defaults only) and fills in defaults.
func Load(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		d := yaml.NewDecoder(bytes.NewReader(data))
		d.KnownFields(true)
		if err := d.Decode(c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&c.Listen, "127.0.0.1:8080")
	def(&c.DataDir, "./data")
	def(&c.Database, filepath.Join(c.DataDir, "builder.db"))
	if c.Worker.Concurrency == 0 {
		c.Worker.Concurrency = 2
	}
	if c.Worker.PollInterval == 0 {
		c.Worker.PollInterval = time.Second
	}
	if c.Worker.Lease == 0 {
		c.Worker.Lease = 30 * time.Second
	}
	if c.Logs.MaxBytesPerAttempt == 0 {
		c.Logs.MaxBytesPerAttempt = 8 << 20
	}
	def(&c.Upstreams.NPMRegistry, "https://registry.npmjs.org")
	def(&c.Upstreams.NodeDist, "https://nodejs.org/dist")
	def(&c.Upstreams.NodeMuslDist, "https://unofficial-builds.nodejs.org/download/release")
	def(&c.Upstreams.AkhookRepository, "https://github.com/aghub-app/akhook")
	def(&c.Upstreams.AkhookRef, "main")
	def(&c.Upstreams.ToolchainImage, "docker.io/library/rust:1-alpine")
	if c.Registry.Enabled {
		def(&c.Registry.Listen, "127.0.0.1:5000")
		def(&c.Registry.StorageDir, filepath.Join(c.DataDir, "registry"))
		def(&c.Registry.PublicHost, c.Registry.Listen)
		def(&c.Registry.CheckURL, "http://"+c.Registry.Listen)
		c.Registry.Password = os.Getenv("SAB_REGISTRY_PASSWORD")
		c.Registry.Secret = os.Getenv("SAB_REGISTRY_HTTP_SECRET")
	}
	if c.Worker.Lease < 3*time.Second {
		return nil, errors.New("worker.lease must be at least 3s")
	}
	for id, s := range c.BuildSecrets {
		if (s.Env == "") == (s.File == "") {
			return nil, fmt.Errorf("buildSecrets.%s: set env or file", id)
		}
	}
	if len(c.Auth.Callers) == 0 && c.Auth.AnonymousCaller == "" {
		return nil, errors.New("auth: configure callers (or anonymousCaller for local development)")
	}
	return c, nil
}

// LoadSecrets reads every build secret now, so a missing one stops the
// builder at start; changed values take effect on restart.
func (c *Config) LoadSecrets() (map[string][]byte, error) {
	out := map[string][]byte{}
	for id, src := range c.BuildSecrets {
		if src.Env != "" {
			v, ok := os.LookupEnv(src.Env)
			if !ok {
				return nil, fmt.Errorf("buildSecrets.%s: environment variable %s is not set", id, src.Env)
			}
			out[id] = []byte(v)
			continue
		}
		v, err := os.ReadFile(src.File)
		if err != nil {
			return nil, fmt.Errorf("buildSecrets.%s: %w", id, err)
		}
		out[id] = v
	}
	return out, nil
}
