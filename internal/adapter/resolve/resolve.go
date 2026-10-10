// Package resolve locks a spec's moving parts when a build is submitted:
// image digests (through the Docker daemon), npm versions, the Node.js
// release and its checksum, akhook's commit and plugin commits.
package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/gitsource"
	"github.com/akarachen/sandboxed-agent-builder/internal/app"
	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
	"github.com/akarachen/sandboxed-agent-builder/internal/plan"
	"github.com/akarachen/sandboxed-agent-builder/internal/plugin"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

// ImageDigester resolves an image reference to its index digest.
type ImageDigester interface {
	IndexDigest(ctx context.Context, ref string) (string, error)
}

const (
	// CodexPackage is the Codex SDK; each of its versions depends on the
	// @openai/codex CLI of the same version, so it locks both.
	CodexPackage = "@openai/codex-sdk"
)

type Resolver struct {
	Images      ImageDigester
	HTTP        *http.Client
	Git         *gitsource.Git
	NPMRegistry string // https://registry.npmjs.org
	NodeDist    string // https://nodejs.org/dist
	AkhookRepo  string // https://github.com/aghub-app/akhook
	AkhookRef   string // main
	Toolchain   string // docker.io/library/rust:1-alpine
}

var _ app.Resolver = (*Resolver)(nil)

// BaseImage is the official image for a family and release.
func BaseImage(family, version string) string {
	switch family {
	case spec.FamilyDebianSlim:
		return "docker.io/library/debian:" + version + "-slim"
	case spec.FamilyDebian:
		return "docker.io/library/debian:" + version
	}
	return ""
}

func invalid(format string, args ...any) error {
	return &domain.SpecError{Problems: []string{fmt.Sprintf(format, args...)}}
}

func (r *Resolver) Resolve(ctx context.Context, s spec.Spec) (spec.Resolved, error) {
	out := spec.Resolved{Contract: plan.Contract, Platform: s.Platform, Scripts: s.Scripts, Output: s.Output, Plugins: []spec.ResolvedPlugin{}, RuntimeEnv: []string{}}

	image := BaseImage(s.Base.Family, s.Base.Version)
	digest, err := r.Images.IndexDigest(ctx, image)
	if err != nil {
		return out, fmt.Errorf("base image %s: %w", image, err)
	}
	out.Base = spec.ResolvedImage{Family: s.Base.Family, Version: s.Base.Version, Image: image, Digest: digest}

	if out.Node, err = r.node(ctx, s.Platform); err != nil {
		return out, err
	}
	if out.Codex, err = r.npm(ctx, CodexPackage, s.Codex.Version); err != nil {
		return out, err
	}

	commit, err := r.Git.ResolveRevision(ctx, r.AkhookRepo, r.AkhookRef)
	if err != nil {
		return out, fmt.Errorf("akhook: %w", err)
	}
	toolchain, err := r.Images.IndexDigest(ctx, r.Toolchain)
	if err != nil {
		return out, fmt.Errorf("toolchain image %s: %w", r.Toolchain, err)
	}
	out.Akhook = spec.ResolvedAkhook{Repository: r.AkhookRepo, Commit: commit, Toolchain: spec.ResolvedImage{Image: r.Toolchain, Digest: toolchain}}

	var read []*plugin.Plugin
	env := map[string]bool{}
	for i, p := range s.Plugins {
		commit, err := r.Git.ResolveRevision(ctx, p.Source, p.Revision)
		if errors.Is(err, gitsource.ErrUnknownRevision) {
			return out, invalid("plugins[%d]: %v", i, err)
		}
		if err != nil {
			return out, fmt.Errorf("plugins[%d]: %w", i, err)
		}
		repo, err := r.Git.Fetch(p.Source, commit)
		if errors.Is(err, gitsource.ErrUnknownRevision) {
			return out, invalid("plugins[%d]: %v", i, err)
		}
		if err != nil {
			return out, fmt.Errorf("plugins[%d]: %w", i, err)
		}
		dir := filepath.Join(repo, filepath.FromSlash(p.Path))
		pl, err := plugin.Read(dir)
		if err != nil {
			return out, invalid("plugins[%d]: %v", i, err)
		}
		rp := spec.ResolvedPlugin{
			Name: pl.Name, Version: pl.Version, Source: p.Source, Revision: p.Revision, Commit: commit, Path: p.Path, SpecVersion: pl.SpecVersion, Hooks: plugin.HookEvents(pl),
		}
		for n := range pl.Servers {
			rp.MCPServers = append(rp.MCPServers, plugin.ServerID(pl.Name, n))
		}
		sort.Strings(rp.MCPServers)
		for _, sk := range pl.Skills {
			rp.Skills = append(rp.Skills, sk.Name)
		}
		for _, e := range plugin.RuntimeEnv(pl) {
			env[e] = true
		}
		out.Plugins = append(out.Plugins, rp)
		read = append(read, pl)
	}
	if err := plugin.CheckSet(read); err != nil {
		return out, invalid("plugins: %v", err)
	}
	sort.Slice(out.Plugins, func(i, j int) bool { return out.Plugins[i].Name < out.Plugins[j].Name })
	for e := range env {
		out.RuntimeEnv = append(out.RuntimeEnv, e)
	}
	sort.Strings(out.RuntimeEnv)
	return out, nil
}

func (r *Resolver) get(ctx context.Context, u string, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

var errNotFound = errors.New("not found")

// npm locks a package version: "latest" takes the latest dist-tag, which must be a stable (non-prerelease) version.
func (r *Resolver) npm(ctx context.Context, pkg, want string) (spec.ResolvedPackage, error) {
	body, err := r.get(ctx, r.NPMRegistry+"/"+strings.Replace(url.PathEscape(pkg), "%40", "@", 1), "application/vnd.npm.install-v1+json")
	if err != nil {
		return spec.ResolvedPackage{}, fmt.Errorf("npm %s: %w", pkg, err)
	}
	var meta struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return spec.ResolvedPackage{}, fmt.Errorf("npm %s: %w", pkg, err)
	}
	version := want
	if want == spec.Latest {
		version = meta.DistTags["latest"]
		if version == "" || strings.Contains(version, "-") {
			return spec.ResolvedPackage{}, fmt.Errorf("npm %s: latest dist-tag %q is not a stable version", pkg, version)
		}
	}
	if _, ok := meta.Versions[version]; !ok {
		return spec.ResolvedPackage{}, invalid("%s: version %s is not published", pkg, version)
	}
	return spec.ResolvedPackage{Package: pkg, Version: version}, nil
}

func nodeArch(platform string) (string, error) {
	switch platform {
	case "linux/amd64":
		return "x64", nil
	case "linux/arm64":
		return "arm64", nil
	}
	return "", invalid("platform %s has no Node.js build", platform)
}

// NodeLine is the Node.js major line images get: the control API and the
// Codex launcher run on it.
const NodeLine = "24"

// node locks the newest NodeLine release for the platform: the official
// latest-v<line>.x checksums name its tarball and sha256.
func (r *Resolver) node(ctx context.Context, platform string) (spec.ResolvedNode, error) {
	arch, err := nodeArch(platform)
	if err != nil {
		return spec.ResolvedNode{}, err
	}
	sums, err := r.get(ctx, fmt.Sprintf("%s/latest-v%s.x/SHASUMS256.txt", r.NodeDist, NodeLine), "")
	if err != nil {
		return spec.ResolvedNode{}, fmt.Errorf("node checksums: %w", err)
	}
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if m := nodeTarRE.FindStringSubmatch(line); m != nil && len(f) == 2 && m[2] == arch {
			return spec.ResolvedNode{Version: m[1], URL: fmt.Sprintf("%s/v%s/%s", r.NodeDist, m[1], f[1]), SHA256: f[0]}, nil
		}
	}
	return spec.ResolvedNode{}, fmt.Errorf("node checksums: no linux-%s tarball", arch)
}

var nodeTarRE = regexp.MustCompile(`node-v(\d+\.\d+\.\d+)-linux-(\w+)\.tar\.gz$`)
