package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// write puts a manifest and, when body is non-empty, an overlay beside it, and
// returns both paths.
func write(t *testing.T, manifest, overlay string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "devctl.yaml")
	require.NoError(t, os.WriteFile(path, []byte(manifest), 0o600))
	overlayPath := filepath.Join(dir, OverlayName)
	if overlay != "" {
		require.NoError(t, os.WriteFile(overlayPath, []byte(overlay), 0o600))
	}
	return path, overlayPath
}

const base = `
dependencies:
  - name: db
    env: DATABASE_URL
    example: postgres://localhost:5432/shop
    kind: tcp
services:
  - name: catalogue
    ports:
      - {name: http, port: 7200, kind: http}
      - {name: grpc, port: 7201, kind: grpc}
    autostart: true
    cmd: go run ./cmd/catalogue
    env:
      DATABASE_URL: "{{ db.address }}"
      LOG_LEVEL: info
  - name: mailer
    cmd: go run ./cmd/mailer
`

func TestNoOverlayLeavesTheManifestAlone(t *testing.T) {
	path, overlayPath := write(t, base, "")
	file, err := LoadWithOverlay(path, overlayPath)
	require.NoError(t, err)
	require.Len(t, file.Services, 2)
	assert.Equal(t, "go run ./cmd/catalogue", file.Services[0].Cmd)
}

func TestOverlayChangesOnePortWithoutRestatingTheService(t *testing.T) {
	path, overlayPath := write(t, base, `
services:
  - name: catalogue
    ports:
      - {name: grpc, port: 9999}
`)
	file, err := LoadWithOverlay(path, overlayPath)
	require.NoError(t, err)

	catalogue := file.Services[0]
	assert.Equal(t, "go run ./cmd/catalogue", catalogue.Cmd, "untouched keys survive")
	require.Len(t, catalogue.Ports, 2, "the other port is not dropped")

	http, ok := catalogue.Port("http")
	require.True(t, ok)
	assert.Equal(t, 7200, http.Number)

	grpc, ok := catalogue.Port("grpc")
	require.True(t, ok)
	assert.Equal(t, 9999, grpc.Number)
	assert.Equal(t, "grpc", grpc.Kind, "the rest of the port survives too")
}

func TestOverlayMergesEnvKeyByKey(t *testing.T) {
	path, overlayPath := write(t, base, `
services:
  - name: catalogue
    env:
      LOG_LEVEL: debug
      EXTRA: yes
`)
	file, err := LoadWithOverlay(path, overlayPath)
	require.NoError(t, err)

	env := file.Services[0].Env
	assert.Equal(t, "debug", env["LOG_LEVEL"], "overridden")
	assert.Equal(t, "yes", env["EXTRA"], "added")
	assert.Equal(t, "{{ db.address }}", env["DATABASE_URL"], "kept")
}

// The case this was built for: the committed manifest says the repository needs
// a database, and each machine says how it gets one.
func TestOverlayCanChangeHowADependencyArrives(t *testing.T) {
	path, overlayPath := write(t, base, `
dependencies:
  - name: db
    example: postgres://rack.local:5432/shop
`)
	file, err := LoadWithOverlay(path, overlayPath)
	require.NoError(t, err)
	assert.Equal(t, "postgres://rack.local:5432/shop", file.Dependencies[0].Example)
	assert.Equal(t, "DATABASE_URL", file.Dependencies[0].Env, "still read from the same variable")
}

func TestOverlayAddsThingsTheManifestDoesNotHave(t *testing.T) {
	path, overlayPath := write(t, base, `
services:
  - name: scratch
    cmd: go run ./cmd/scratch
tasks:
  - name: reset
    cmd: ./scripts/reset.sh
`)
	file, err := LoadWithOverlay(path, overlayPath)
	require.NoError(t, err)
	require.Len(t, file.Services, 3)
	assert.Equal(t, "scratch", file.Services[2].Name)
	require.Len(t, file.Tasks, 1)
	assert.Equal(t, "reset", file.Tasks[0].Name)
}

func TestOverlayCanTurnAFlagOff(t *testing.T) {
	path, overlayPath := write(t, base, `
services:
  - name: catalogue
    autostart: false
`)
	file, err := LoadWithOverlay(path, overlayPath)
	require.NoError(t, err)
	assert.False(t, file.Services[0].Autostart,
		"merging happens as data, so false in the overlay means false")
}

// A dependency's day-to-day mode is a per-machine choice, so the overlay sets
// the default without restating the modes the manifest declares.
func TestOverlayCanChangeAMultiModeDefault(t *testing.T) {
	manifest := `
dependencies:
  - name: platform
    default: local
    modes:
      - name: local
        peer: {id: platform, service: gateway, port: http}
      - name: staging
        port: 7100
        forward: {cmd: "kubectl port-forward svc/gateway {{ platform.port.number }}:8080"}
services:
  - name: ui
    cmd: npm run dev
    env:
      PLATFORM_ADDR: "{{ platform.address }}"
`
	path, overlayPath := write(t, manifest, `
dependencies:
  - name: platform
    default: staging
`)
	file, err := LoadWithOverlay(path, overlayPath)
	require.NoError(t, err)
	assert.Equal(t, "staging", file.Dependencies[0].DefaultMode(),
		"the machine picks which source it uses day to day")
	require.Len(t, file.Dependencies[0].Modes, 2, "the modes themselves survive the merge")
}

func TestOverlayIsValidatedWithTheManifestAndNamedWhenItBreaksIt(t *testing.T) {
	path, overlayPath := write(t, base, `
services:
  - name: catalogue
    ports:
      - {name: grpc, port: 70000}
`)
	_, err := LoadWithOverlay(path, overlayPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), OverlayName, "the overlay is what changed, so say so")
}
