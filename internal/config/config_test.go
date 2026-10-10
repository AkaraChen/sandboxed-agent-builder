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
	if s, err := c.LoadSecrets(); err != nil || string(s["npm_token"]) != "tok-value" {
		t.Fatalf("%q %v", s, err)
	}
}
