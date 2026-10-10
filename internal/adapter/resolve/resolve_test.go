package resolve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akarachen/sandboxed-agent-builder/internal/domain"
	"github.com/akarachen/sandboxed-agent-builder/internal/spec"
)

func upstream(t *testing.T, latest string) *Resolver {
	mux := http.NewServeMux()
	mux.HandleFunc("/npm/", func(w http.ResponseWriter, r *http.Request) {
		// npm takes the scoped name with an escaped slash.
		if r.URL.EscapedPath() != "/npm/@botiverse%2Foar-cli" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"dist-tags":{"latest":"` + latest + `","next":"0.52.0-rc.1"},"versions":{"0.50.0":{},"0.51.0":{},"0.52.0-rc.1":{}}}`))
	})
	mux.HandleFunc("/node/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"version":"v25.1.0","files":["linux-x64"]},{"version":"v24.9.0","files":["linux-x64"]},{"version":"v24.10.0","files":["linux-arm64"]},{"version":"v24.8.1","files":["linux-x64"]}]`))
	})
	mux.HandleFunc("/node/v24.9.0/SHASUMS256.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("aaaa  node-v24.9.0-linux-x64.tar.xz\nbbbb  node-v24.9.0-linux-x64.tar.gz\n"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &Resolver{HTTP: ts.Client(), NPMRegistry: ts.URL + "/npm", NodeDist: ts.URL + "/node"}
}

func TestNPM(t *testing.T) {
	ctx := context.Background()
	r := upstream(t, "0.51.0")
	if p, err := r.npm(ctx, OarPackage, spec.LatestStable); err != nil || p.Version != "0.51.0" {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err := r.npm(ctx, OarPackage, "0.50.0"); err != nil || p.Version != "0.50.0" {
		t.Fatalf("exact: %+v %v", p, err)
	}
	var se *domain.SpecError
	if _, err := r.npm(ctx, OarPackage, "9.9.9"); !errors.As(err, &se) {
		t.Fatalf("unpublished: %v", err)
	}
	if _, err := upstream(t, "0.52.0-rc.1").npm(ctx, OarPackage, spec.LatestStable); err == nil {
		t.Fatal("prerelease latest accepted as stable")
	}
}

func TestNode(t *testing.T) {
	r := upstream(t, "0.51.0")
	n, err := r.node(context.Background(), "24", "linux/amd64", false)
	if err != nil || n.Version != "24.9.0" || n.SHA256 != "bbbb" || n.URL != r.NodeDist+"/v24.9.0/node-v24.9.0-linux-x64.tar.gz" {
		t.Fatalf("%+v %v", n, err)
	}
	var se *domain.SpecError
	if _, err := r.node(context.Background(), "26", "linux/amd64", false); !errors.As(err, &se) {
		t.Fatalf("missing line: %v", err)
	}
}
