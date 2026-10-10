// Package registry embeds a Distribution registry (the CNCF
// distribution/distribution v3 handlers and filesystem storage) and
// publishes attempt images to it through the Docker daemon.
//
// The registry is a separate listener from the API. The daemon pushes to
// PublicHost, so PublicHost must be reachable from the daemon; callers on
// other machines pull from it too, which needs TLS (or an
// insecure-registries entry on their daemon) unless it is a loopback
// address. Blob and manifest storage is the configured directory; the
// builder never deletes from it. GC removes unreferenced blobs offline.
package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	"github.com/distribution/distribution/v3/registry/storage"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	"github.com/moby/moby/api/types/registry"
	"github.com/sirupsen/logrus"

	_ "github.com/distribution/distribution/v3/registry/auth/htpasswd"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"

	"github.com/akarachen/sandboxed-agent-builder/internal/adapter/docker"
	"github.com/akarachen/sandboxed-agent-builder/internal/app"
)

// Config is the registry section of the builder's configuration. The
// builder's push password and the upload-signing secret come from
// SAB_REGISTRY_PASSWORD and SAB_REGISTRY_HTTP_SECRET.
type Config struct {
	Listen     string `yaml:"listen"`
	StorageDir string `yaml:"storageDir"`
	// PublicHost is host:port as the daemon and callers address it.
	PublicHost string `yaml:"publicHost"`
	// CheckURL is how the builder itself reaches the registry
	// (http://127.0.0.1:5000), to confirm manifests.
	CheckURL string `yaml:"checkUrl"`
	// Htpasswd enables basic auth; Username and Password are the builder's
	// own push credentials (an entry in the file).
	Htpasswd string `yaml:"htpasswd"`
	Username string `yaml:"username"`
	Password string `yaml:"-"`
	TLSCert  string `yaml:"tlsCert"`
	TLSKey   string `yaml:"tlsKey"`
	// Secret signs upload state between requests; set it so uploads
	// survive restarts.
	Secret string `yaml:"-"`
}

func (c Config) distribution() *configuration.Configuration {
	cfg := &configuration.Configuration{}
	cfg.Version = "0.1"
	cfg.Log.Level = "warn"
	cfg.Log.AccessLog.Disabled = true
	cfg.Storage = configuration.Storage{
		"filesystem": configuration.Parameters{"rootdirectory": c.StorageDir},
		"delete":     configuration.Parameters{"enabled": true},
	}
	cfg.HTTP.Addr = c.Listen
	cfg.HTTP.Secret = c.Secret
	cfg.HTTP.Headers = http.Header{"X-Content-Type-Options": []string{"nosniff"}}
	if c.Htpasswd != "" {
		cfg.Auth = configuration.Auth{"htpasswd": configuration.Parameters{"realm": "sandboxed-agent-builder", "path": c.Htpasswd}}
	}
	return cfg
}

// Server is the running registry.
type Server struct {
	srv *http.Server
	ln  net.Listener
}

// Start serves the registry until Shutdown.
func Start(ctx context.Context, c Config, log *slog.Logger) (*Server, error) {
	logrus.SetLevel(logrus.WarnLevel)
	logrus.SetOutput(io.Discard)
	handler := handlers.NewApp(ctx, c.distribution())
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return nil, err
	}
	s := &Server{srv: &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}, ln: ln}
	go func() {
		var err error
		if c.TLSCert != "" {
			s.srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			err = s.srv.ServeTLS(ln, c.TLSCert, c.TLSKey)
		} else {
			err = s.srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("registry stopped", "err", err)
		}
	}()
	log.Info("registry listening", "addr", ln.Addr().String(), "public_host", c.PublicHost, "storage", c.StorageDir, "auth", c.Htpasswd != "", "tls", c.TLSCert != "")
	return s, nil
}

func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// GC removes blobs no manifest references (and, with untagged, manifests
// no tag references). Run it with the registry stopped.
func GC(ctx context.Context, c Config, dryRun, untagged bool) error {
	cfg := c.distribution()
	driver, err := factory.Create(ctx, cfg.Storage.Type(), cfg.Storage.Parameters())
	if err != nil {
		return err
	}
	reg, err := storage.NewRegistry(ctx, driver)
	if err != nil {
		return err
	}
	return storage.MarkAndSweep(ctx, driver, reg, storage.GCOpts{DryRun: dryRun, RemoveUntagged: untagged})
}

// Publisher pushes through the daemon and confirms the manifest in the
// registry before reporting success.
type Publisher struct {
	Config Config
	Daemon *docker.Daemon
	HTTP   *http.Client
}

var _ app.Publisher = (*Publisher)(nil)

func (p *Publisher) Publish(ctx context.Context, img app.Image, repository, tag string, log app.LogFunc) (app.Published, error) {
	target := p.Config.PublicHost + "/" + repository + ":" + tag
	log("builder", "pushing "+target)
	auth := registry.AuthConfig{Username: p.Config.Username, Password: p.Config.Password, ServerAddress: p.Config.PublicHost}
	digest, err := p.Daemon.Push(ctx, img, target, auth, log)
	if err != nil {
		return app.Published{}, fmt.Errorf("push %s: %w", target, err)
	}
	found, err := p.Find(ctx, repository, tag)
	if err != nil {
		return app.Published{}, fmt.Errorf("confirming %s: %w", target, err)
	}
	if found == nil {
		return app.Published{}, fmt.Errorf("the daemon reported %s pushed, but the registry has no manifest for it", target)
	}
	if found.Digest != digest {
		return app.Published{}, fmt.Errorf("the daemon pushed %s but the registry has %s under %s", digest, found.Digest, target)
	}
	return *found, nil
}

const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

func (p *Publisher) Find(ctx context.Context, repository, tag string) (*app.Published, error) {
	u := strings.TrimSuffix(p.Config.CheckURL, "/") + "/v2/" + repository + "/manifests/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestTypes)
	if p.Config.Username != "" {
		req.SetBasicAuth(p.Config.Username, p.Config.Password)
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("HEAD %s: %s", u, resp.Status)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return nil, fmt.Errorf("HEAD %s: no Docker-Content-Digest", u)
	}
	repo := p.Config.PublicHost + "/" + repository
	return &app.Published{Repository: repo, Digest: digest, Ref: repo + "@" + digest}, nil
}
