package config

import (
	"testing"
	"time"
)

func TestExampleLoads(t *testing.T) {
	c, err := Load("../../deploy/builder.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Worker.Lease != 30*time.Second || c.Auth.Callers[0].ID != "axp" || c.BuildSecrets["npm_token"].Env != "NPM_TOKEN" {
		t.Fatalf("%+v", c)
	}
	t.Setenv("NPM_TOKEN", "tok-value")
	if v, err := c.Secrets().Get("npm_token"); err != nil || string(v) != "tok-value" {
		t.Fatalf("%q %v", v, err)
	}
}
