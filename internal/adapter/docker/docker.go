// Package docker is the Executor on a Docker daemon: builds go through the
// daemon's embedded BuildKit, reached over the Docker socket the same way
// buildx's docker driver does (/grpc and /session hijacked endpoints), and
// are exported into the daemon's image store with the moby exporter.
package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/moby/moby/api/types/registry"
	dockerclient "github.com/moby/moby/client"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"

	"github.com/akarachen/sandboxed-agent-builder/internal/app"
)

// Labels on every image the builder makes; reconciliation trusts an image
// for an attempt only when they name it.
const (
	LabelBuild      = "io.sandboxed-agent-builder.build-id"
	LabelAttempt    = "io.sandboxed-agent-builder.attempt-id"
	LabelSpecDigest = "io.sandboxed-agent-builder.spec-digest"
	LabelContract   = "io.sandboxed-agent-builder.contract"
)

type Daemon struct {
	api *dockerclient.Client
}

var _ app.Executor = (*Daemon)(nil)

// New connects to the daemon at host ("" uses DOCKER_HOST or the default
// socket).
func New(host string) (*Daemon, error) {
	opts := []dockerclient.Opt{dockerclient.FromEnv}
	if host != "" {
		opts = append(opts, dockerclient.WithHost(host))
	}
	api, err := dockerclient.New(opts...)
	if err != nil {
		return nil, err
	}
	return &Daemon{api: api}, nil
}

func (d *Daemon) Close() error { return d.api.Close() }

// Platform is the daemon's native platform, os/arch.
func (d *Daemon) Platform(ctx context.Context) (string, error) {
	info, err := d.api.Info(ctx, dockerclient.InfoOptions{})
	if err != nil {
		return "", err
	}
	arch := info.Info.Architecture
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	return info.Info.OSType + "/" + arch, nil
}

// Ping checks the daemon answers.
func (d *Daemon) Ping(ctx context.Context) error {
	_, err := d.api.Ping(ctx, dockerclient.PingOptions{})
	return err
}

func (d *Daemon) buildkit(ctx context.Context) (*client.Client, error) {
	return client.New(ctx, "",
		client.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return d.api.DialHijack(ctx, "/grpc", "h2c", nil)
		}),
		client.WithSessionDialer(func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {
			return d.api.DialHijack(ctx, "/session", proto, meta)
		}),
	)
}

// Build runs the Dockerfile frontend on req.Context. Cancelling ctx
// cancels the solve; BuildKit stops the build when the client goes away,
// and Build returns after Solve did.
func (d *Daemon) Build(ctx context.Context, req app.BuildRequest, log app.LogFunc) (app.Image, error) {
	bk, err := d.buildkit(ctx)
	if err != nil {
		return app.Image{}, fmt.Errorf("connecting to BuildKit through the daemon: %w", err)
	}
	defer bk.Close()

	local, err := fsutil.NewFS(req.Context)
	if err != nil {
		return app.Image{}, err
	}
	attrs := map[string]string{
		"filename":                 "Dockerfile",
		"platform":                 req.Platform,
		"label:" + LabelBuild:      req.BuildID,
		"label:" + LabelAttempt:    req.AttemptID,
		"label:" + LabelSpecDigest: req.SpecDigest,
		"label:" + LabelContract:   "1",
	}
	if req.NoCache {
		attrs["no-cache"] = ""
	}
	opt := client.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
		LocalMounts:   map[string]fsutil.FS{"context": local, "dockerfile": local},
		Exports: []client.ExportEntry{{
			Type:  "moby",
			Attrs: map[string]string{"name": req.Tag},
		}},
		Session: []session.Attachable{secretsprovider.FromMap(req.Secrets)},
	}

	ch := make(chan *client.SolveStatus)
	pr, pw := io.Pipe()
	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		_, err := bk.Solve(egCtx, nil, opt, ch)
		return err
	})
	eg.Go(func() error {
		defer pw.Close()
		display, err := progressui.NewDisplay(pw, progressui.PlainMode)
		if err != nil {
			for range ch {
			}
			return err
		}
		_, err = display.UpdateFrom(context.WithoutCancel(egCtx), ch)
		return err
	})
	var copyWG sync.WaitGroup
	copyWG.Go(func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				log("buildkit", string(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	})
	err = eg.Wait()
	pr.Close()
	copyWG.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return app.Image{}, fmt.Errorf("build stopped: %w", context.Cause(ctx))
		}
		return app.Image{}, err
	}

	img, err := d.inspect(context.WithoutCancel(ctx), req.Tag)
	if err != nil {
		return app.Image{}, fmt.Errorf("the build finished but its image is not in the daemon: %w", err)
	}
	if img == nil {
		return app.Image{}, errors.New("the build finished but its image is not in the daemon")
	}
	return *img, nil
}

func (d *Daemon) inspect(ctx context.Context, ref string) (*app.Image, error) {
	res, err := d.api.ImageInspect(ctx, ref)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &app.Image{ID: res.ID, Ref: ref, Platform: res.Os + "/" + res.Architecture}, nil
}

func isNotFound(err error) bool {
	return err != nil && (strings.Contains(strings.ToLower(err.Error()), "no such image") || strings.Contains(strings.ToLower(err.Error()), "not found"))
}

// FindAttemptImage looks the attempt's tag up and checks its labels.
func (d *Daemon) FindAttemptImage(ctx context.Context, buildID, attemptID string) (*app.Image, error) {
	ref := app.DaemonRef(buildID, attemptID)
	res, err := d.api.ImageInspect(ctx, ref)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if res.Config == nil || res.Config.Labels[LabelAttempt] != attemptID || res.Config.Labels[LabelBuild] != buildID {
		return nil, nil
	}
	return &app.Image{ID: res.ID, Ref: ref, Platform: res.Os + "/" + res.Architecture}, nil
}

// IndexDigest resolves an image reference to its registry (index)
// digest through the daemon, without pulling it.
func (d *Daemon) IndexDigest(ctx context.Context, ref string) (string, error) {
	res, err := d.api.DistributionInspect(ctx, ref, dockerclient.DistributionInspectOptions{})
	if err != nil {
		return "", err
	}
	return res.Descriptor.Digest.String(), nil
}

var pushedDigest = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)

// Push tags img as target, pushes it with auth and returns the manifest
// digest the daemon reported.
func (d *Daemon) Push(ctx context.Context, img app.Image, target string, auth registry.AuthConfig, log app.LogFunc) (string, error) {
	if _, err := d.api.ImageTag(ctx, dockerclient.ImageTagOptions{Source: img.ID, Target: target}); err != nil {
		return "", fmt.Errorf("tagging %s: %w", target, err)
	}
	encoded, err := json.Marshal(auth)
	if err != nil {
		return "", err
	}
	resp, err := d.api.ImagePush(ctx, target, dockerclient.ImagePushOptions{RegistryAuth: base64.URLEncoding.EncodeToString(encoded)})
	if err != nil {
		return "", err
	}
	digest := ""
	for msg, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return "", err
		}
		if msg.Error != nil {
			return "", errors.New(msg.Error.Message)
		}
		if msg.Aux != nil {
			var aux struct {
				Digest string `json:"Digest"`
			}
			if json.Unmarshal(*msg.Aux, &aux) == nil && aux.Digest != "" {
				digest = aux.Digest
			}
		}
		// Daemons with the containerd image store report the digest only
		// in the final status line.
		if m := pushedDigest.FindStringSubmatch(msg.Status); m != nil && digest == "" {
			digest = m[1]
		}
		if msg.Status != "" && msg.Progress == nil {
			line := msg.Status
			if msg.ID != "" {
				line = msg.ID + ": " + line
			}
			log("push", line+"\n")
		}
	}
	if digest == "" {
		return "", errors.New("the daemon reported no manifest digest for the push")
	}
	return digest, nil
}
