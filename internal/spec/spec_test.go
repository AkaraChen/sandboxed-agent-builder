package spec

import (
	"errors"
	"strings"
	"testing"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
)

var policy = Policy{Platforms: []string{"linux/amd64"}, Secrets: []string{"npm"}, PluginSource: func(s string) bool { return strings.HasPrefix(s, "https://") }}

func TestDefaults(t *testing.T) {
	var s Spec
	if err := s.Normalize(policy); err != nil {
		t.Fatal(err)
	}
	if s.Base.Family != FamilyDebianSlim || s.Base.Version != "trixie" || s.Platform != "linux/amd64" ||
		s.Node.Version != "24" || s.Agent.Type != AgentCodex || s.Agent.Codex.Version != Latest || s.Oar.Version != LatestStable || s.Output.Type != OutputLocal {
		t.Fatalf("%+v", s)
	}
}

func TestRejections(t *testing.T) {
	for name, s := range map[string]Spec{
		"ubuntu":         {Base: Base{Family: "ubuntu"}},
		"alpine":         {Base: Base{Family: "alpine"}},
		"debian release": {Base: Base{Family: FamilyDebian, Version: "jessie"}},
		"platform":       {Platform: "linux/s390x"},
		"old node":       {Node: Version{Version: "22"}},
		"codex range":    {Agent: Agent{Codex: &CodexConfig{Version: "^1.0.0"}}},
		"agent type":     {Agent: Agent{Type: "claude"}},
		"plugin source":  {Plugins: []Plugin{{Source: "git@github.com:x/y", Revision: "main"}}},
		"plugin path":    {Plugins: []Plugin{{Source: "https://x/y", Revision: "main", Path: "../up"}}},
		"no revision":    {Plugins: []Plugin{{Source: "https://x/y"}}},
		"unknown secret": {Scripts: Scripts{Build: []Script{{Name: "a", Run: "b", Secrets: []string{"other"}}}}},
		"startup secret": {Scripts: Scripts{Startup: []Script{{Name: "a", Run: "b", Secrets: []string{"npm"}}}}},
		"empty script":   {Scripts: Scripts{Build: []Script{{Name: "a"}}}},
		"registry off":   {Output: Output{Type: OutputRegistry, Repository: "a/b"}},
	} {
		err := s.Normalize(policy)
		var se *domain.SpecError
		if !errors.As(err, &se) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCanonicalHashIgnoresOrder(t *testing.T) {
	a, _ := Hash(map[string]any{"a": 1, "b": []any{"x", map[string]any{"d": 1, "c": 2}}})
	b, _ := Hash(map[string]any{"b": []any{"x", map[string]any{"c": 2, "d": 1}}, "a": 1})
	c, _ := Hash(map[string]any{"b": []any{map[string]any{"c": 2, "d": 1}, "x"}, "a": 1})
	if a != b || a == c {
		t.Fatalf("%s %s %s", a, b, c)
	}
}
