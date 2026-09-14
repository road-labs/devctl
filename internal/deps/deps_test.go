package deps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/road-labs/devctl/internal/config"
	"github.com/road-labs/devctl/internal/ports"
)

func TestResolveReadsTheEnvFileAndPrefersTheProcessEnvironment(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, EnvFile), []byte("# comment\nexport MONGO_DSN=\"mongodb://file:27017/platform\"\nOTHER=x\n"), 0o600))
	deps := []config.Dependency{{Name: "mongo", Env: "MONGO_DSN", Kind: "mongo"}}

	t.Setenv("MONGO_DSN", "")
	values, err := Resolve(root, deps)
	require.NoError(t, err)
	assert.Equal(t, "mongodb://file:27017/platform", values["mongo.address"])

	t.Setenv("MONGO_DSN", "mongodb://env:27017/platform")
	values, err = Resolve(root, deps)
	require.NoError(t, err)
	assert.Equal(t, "mongodb://env:27017/platform", values["mongo.address"])
}

func TestResolveSaysWhatToAddWhenMissing(t *testing.T) {
	t.Setenv("MONGO_DSN", "")
	_, err := Resolve(t.TempDir(), []config.Dependency{{Name: "mongo", Env: "MONGO_DSN", Example: "mongodb://localhost:27017/platform", Description: "MongoDB"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MONGO_DSN=mongodb://localhost:27017/platform")
	assert.Contains(t, err.Error(), EnvFile)
}

func TestResolveTreatsOptionalAsUnconfiguredAndCheckOnlyWarns(t *testing.T) {
	t.Setenv("BILLING_ADDR", "")
	t.Setenv("PRICING_ADDR", "127.0.0.1:1") // nothing listens there
	deps := []config.Dependency{
		{Name: "billing", Env: "BILLING_ADDR", Optional: true},
		{Name: "pricing", Env: "PRICING_ADDR", Optional: true},
		{Name: "chargenow", Port: 9903, Forward: &config.Forward{Cmd: "true"}, Optional: true},
	}
	values, err := Resolve(t.TempDir(), deps)
	require.NoError(t, err, "a missing optional dependency is not an error")
	assert.Equal(t, "", values["billing.address"])
	_, forwardedHasNoValue := values["chargenow.address"]
	assert.False(t, forwardedHasNoValue, "forwarded dependencies are referenced by port, not address")

	warnings, err := Check(context.Background(), deps, values)
	require.NoError(t, err, "an unreachable optional dependency only warns")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "pricing")
}

func TestCheckFailsOnAnUnreachableRequiredDependency(t *testing.T) {
	deps := []config.Dependency{{Name: "mongo", Env: "MONGO_DSN", Kind: "tcp"}}
	_, err := Check(context.Background(), deps, ports.Values{"mongo.address": "127.0.0.1:1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mongo")
}

func TestHostPort(t *testing.T) {
	for in, want := range map[string]string{
		"mongodb://mongo.lab.example/platform":     "mongo.lab.example:27017",
		"mongodb://localhost:27018/platform":       "localhost:27018",
		"mongodb://user:pw@localhost:27017/x":      "localhost:27017",
		"localhost:6379":                           "localhost:6379",
		"http://localhost:8080":                    "localhost:8080",
		"mongodb+srv://cluster.example/platform":   "",
		"mongodb://a:27017,b:27017/platform?rs=x0": "",
	} {
		got, ok := HostPort(in)
		assert.Equal(t, want != "", ok, in)
		assert.Equal(t, want, got, in)
	}
}

func TestRedactHidesPasswords(t *testing.T) {
	assert.Equal(t, "mongodb://user:***@localhost:27017/platform", Redact("mongodb://user:secret@localhost:27017/platform"))
	assert.Equal(t, "mongodb://localhost:27017/platform", Redact("mongodb://localhost:27017/platform"))
}
