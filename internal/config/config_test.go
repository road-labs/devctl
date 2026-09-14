package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The example manifest in testdata is the reference for every feature the format
// has. It is loaded here so a change to the format has to keep the example true.
func TestExampleManifestLoads(t *testing.T) {
	file, err := Load(filepath.Join("..", "..", "testdata", "devctl.yaml"))
	require.NoError(t, err)

	byName := map[string]Service{}
	for _, s := range file.Services {
		byName[s.Name] = s
	}
	require.Len(t, file.Services, 3)

	catalogue, ok := byName["catalogue"]
	require.True(t, ok)
	require.Len(t, catalogue.Ports, 3)
	assert.Equal(t,
		Port{Name: "http", Number: 7200, Kind: "http", Fixed: true, Description: catalogue.Ports[0].Description},
		catalogue.Ports[0])

	grpc, ok := catalogue.Port("grpc")
	require.True(t, ok)
	assert.Equal(t, 7201, grpc.Number)
	assert.Equal(t, "grpc", grpc.Kind)
	assert.False(t, grpc.Fixed, "a port others resolve through the table is free to move")
	assert.False(t, catalogue.FixedPorts, "pinning is per port here, not per service")

	assert.Equal(t, Listen{Env: "HTTP_ADDR", Format: "addr"}, catalogue.Listen["http"],
		"the plain form passes an address to bind")
	assert.Equal(t, Listen{Env: "PORT", Format: "number"}, byName["storefront"].Listen["web"],
		"the long form passes a bare number, for programs that want only that")

	assert.Equal(t, []string{"services/catalogue", "libs"}, catalogue.Watch)
	assert.Empty(t, byName["storefront"].Watch, "no watch list means no auto-restart")
	assert.Equal(t, []string{"catalogue"}, byName["storefront"].DependsOn)
	assert.Empty(t, byName["mailer"].Ports, "a worker needs no ports")

	require.Len(t, file.Dependencies, 2)
	machine, forwarded := file.Dependencies[0], file.Dependencies[1]
	assert.Equal(t, "db", machine.Name)
	assert.Equal(t, "DATABASE_URL", machine.Env)
	assert.False(t, machine.Forwarded(), "the machine provides this one")
	assert.False(t, machine.Optional)
	assert.True(t, forwarded.Forwarded())
	assert.True(t, forwarded.Optional, "a forwarded dependency is optional by nature, without saying so")
	assert.Contains(t, forwarded.Forward.Cmd, "{{ payments.port.number }}",
		"the forward states its local port once, by referring to itself")

	assert.Equal(t, "{{ db.address }}", catalogue.Env["DATABASE_URL"])
	assert.Equal(t, "{{ payments.port }}", catalogue.Env["PAYMENTS_ADDR"])
	assert.Equal(t, "{{ catalogue.http.url }}", byName["storefront"].Env["CATALOGUE_URL"])

	require.Len(t, file.Tasks, 1)
	seed := file.Tasks[0]
	assert.Equal(t, "seed", seed.Name)
	assert.Equal(t, "{{ db.address }}", seed.Env["DATABASE_URL"])
	assert.Equal(t, catalogue.Env["API_KEY"], seed.Env["API_KEY"],
		"one value, stated once as a YAML anchor")
	assert.NotEmpty(t, seed.Env["API_KEY"])
}

func TestValidation(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "services.yaml")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}

	for name, body := range map[string]string{
		"no services":             "services: []\n",
		"duplicate name":          "services:\n  - name: a\n    cmd: x\n  - name: a\n    cmd: x\n",
		"no cmd":                  "services:\n  - name: a\n    dir: x\n",
		"unknown dependency":      "services:\n  - name: a\n    depends_on: [b]\n    cmd: x\n",
		"bad port":                "services:\n  - name: a\n    ports: [{name: grpc, port: 70000}]\n    cmd: x\n",
		"duplicate port name":     "services:\n  - name: a\n    ports: [{name: grpc, port: 1}, {name: grpc, port: 2}]\n    cmd: x\n",
		"unknown port kind":       "services:\n  - name: a\n    ports: [{name: grpc, port: 1, kind: udp}]\n    cmd: x\n",
		"listen on undeclared":    "services:\n  - name: a\n    ports: [{name: grpc, port: 1}]\n    cmd: x\n    listen: {http: HTTP_ADDR}\n",
		"dependency without env":  "dependencies:\n  - name: mongo\nservices:\n  - name: a\n    cmd: x\n",
		"dependency bad kind":     "dependencies:\n  - name: mongo\n    env: X\n    kind: udp\nservices:\n  - name: a\n    cmd: x\n",
		"task without cmd":        "services:\n  - name: a\n    cmd: x\ntasks:\n  - name: t\n",
		"task unknown dependency": "services:\n  - name: a\n    cmd: x\ntasks:\n  - name: t\n    cmd: x\n    depends_on: [b]\n",
		"task named like service": "services:\n  - name: a\n    cmd: x\ntasks:\n  - name: a\n    cmd: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, body))
			assert.Error(t, err)
		})
	}
}
