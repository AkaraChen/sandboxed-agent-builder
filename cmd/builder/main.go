// Command builder is sandboxed-agent-builder's composition root: it reads
// the configuration and wires the API, the worker (which also reconciles), the
// SQLite store, the Docker executor and the optional registry together.
//
//	builder [serve] -config builder.yaml
//	builder registry-gc -config builder.yaml [-dry-run] [-untagged]
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/docker"
	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/gitsource"
	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/postgres"
	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/registry"
	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/resolve"
	"github.com/akarachen/sandboxed-agent-builder/internal/api"
	"github.com/akarachen/sandboxed-agent-builder/internal/app"
	"github.com/akarachen/sandboxed-agent-builder/internal/config"
	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log, args)
	case "registry-gc":
		err = registryGC(args)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("builder failed", "err", err)
		os.Exit(1)
	}
}

func registryGC(args []string) error {
	fs := flag.NewFlagSet("registry-gc", flag.ExitOnError)
	path := fs.String("config", "", "configuration file")
	dry := fs.Bool("dry-run", false, "only report what would be removed")
	untagged := fs.Bool("untagged", false, "also remove manifests no tag references")
	_ = fs.Parse(args)
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if !c.Registry.Enabled {
		return errors.New("registry is not enabled in the configuration")
	}
	return registry.GC(context.Background(), c.Registry.Config, *dry, *untagged)
}

func setupTracing(ctx context.Context) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("sandboxed-agent-builder"))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func serve(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", os.Getenv("SAB_CONFIG"), "configuration file")
	_ = fs.Parse(args)
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := setupTracing(ctx)
	if err != nil {
		return err
	}
	defer shutdownTracing(context.Background())

	for _, dir := range []string{c.DataDir, filepath.Join(c.DataDir, "work"), filepath.Join(c.DataDir, "git")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	store, err := postgres.Open(ctx, c.Database)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer store.Close()

	daemon, err := docker.New(c.Docker.Host)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer daemon.Close()
	platform, err := daemon.Platform(ctx)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}

	git := &gitsource.Git{Dir: filepath.Join(c.DataDir, "git"), AllowFile: c.Plugins.AllowFileSources}
	httpClient := &http.Client{Timeout: 60 * time.Second}

	var publisher app.Publisher
	if c.Registry.Enabled {
		rc := c.Registry.Config
		if err := os.MkdirAll(rc.StorageDir, 0o700); err != nil {
			return err
		}
		if rc.Secret == "" {
			log.Warn("registry: SAB_REGISTRY_HTTP_SECRET is not set; uploads in progress do not survive a restart")
			b := make([]byte, 32)
			_, _ = rand.Read(b)
			rc.Secret = hex.EncodeToString(b)
		}
		reg, err := registry.Start(ctx, rc, log)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		defer reg.Shutdown(context.Background())
		publisher = &registry.Publisher{Config: rc, Daemon: daemon, HTTP: httpClient}
	}

	secrets, err := c.LoadSecrets()
	if err != nil {
		return err
	}
	policy := spec.Policy{
		Platforms: []string{platform}, Secrets: slices.Sorted(maps.Keys(secrets)),
		Registry: c.Registry.Enabled, PluginSource: git.Allowed,
	}
	resolver := &resolve.Resolver{
		Images: daemon, HTTP: httpClient, Git: git,
		NPMRegistry: c.Upstreams.NPMRegistry, NodeDist: c.Upstreams.NodeDist,
		AkhookRepo: c.Upstreams.AkhookRepository, AkhookRef: c.Upstreams.AkhookRef, Toolchain: c.Upstreams.ToolchainImage,
	}
	service := &app.Service{Store: store, Resolver: resolver, Policy: func() spec.Policy { return policy }, Log: log}

	workerID := c.Worker.ID
	if workerID == "" {
		host, _ := os.Hostname()
		workerID = fmt.Sprintf("%s-%d-%s", host, os.Getpid(), domain.NewID("w")[2:10])
	}
	worker := &app.Worker{
		Store: store, Fetch: git, Executor: daemon, Publisher: publisher, Secrets: secrets, Log: log,
		Config: app.WorkerConfig{
			ID: workerID, Concurrency: c.Worker.Concurrency, Poll: c.Worker.PollInterval, Lease: c.Worker.Lease,
			WorkDir: filepath.Join(c.DataDir, "work"), MaxLogBytes: c.Logs.MaxBytesPerAttempt,
		},
	}

	var callers []api.Caller
	for _, cl := range c.Auth.Callers {
		callers = append(callers, api.Caller{ID: cl.ID, TokenSHA256: strings.ToLower(cl.TokenSHA256)})
	}
	server := &api.Server{
		Service: service, Callers: callers, Anonymous: c.Auth.AnonymousCaller, Log: log,
		Ready: func(ctx context.Context) error {
			if err := store.Ping(ctx); err != nil {
				return fmt.Errorf("database: %w", err)
			}
			if err := daemon.Ping(ctx); err != nil {
				return fmt.Errorf("docker: %w", err)
			}
			return nil
		},
	}
	httpServer := &http.Server{Addr: c.Listen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}

	done := make(chan struct{}, 1)
	go func() { worker.Run(ctx); done <- struct{}{} }()
	errc := make(chan error, 1)
	go func() {
		log.Info("builder listening", "addr", c.Listen, "worker_id", workerID, "platform", platform, "registry", c.Registry.Enabled)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err = <-errc:
		stop()
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	<-done
	return err
}
