package main

import (
	"path/filepath"
	"testing"

	"github.com/road-labs/devctl/internal/config"
	"github.com/road-labs/devctl/internal/ports"
)

// The example projects are meant to be run, so their manifests must stay valid
// as the format changes, the same guarantee testdata gives. They are a separate
// Go module, so the main build never reaches their code; this reads their
// manifests as files and holds them to the schema.
func TestExampleProjectsAreValid(t *testing.T) {
	for _, name := range []string{"platform", "billing"} {
		path := filepath.Join("example", name, "devctl.yaml")
		file, err := config.Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := ports.Validate(file); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}
