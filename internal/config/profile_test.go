package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T) *File {
	t.Helper()
	f, err := Load(filepath.Join("..", "..", "testdata", "devctl.yaml"))
	require.NoError(t, err)
	return f
}

func names(f *File) []string {
	var out []string
	for _, d := range f.Dependencies {
		out = append(out, d.Name)
	}
	for _, s := range f.Services {
		out = append(out, s.Name)
	}
	for _, t := range f.Tasks {
		out = append(out, t.Name)
	}
	return out
}

// No target is the whole manifest, which is what devctl did before profiles
// existed and has to keep doing.
func TestSelectWithoutTargetsIsEverything(t *testing.T) {
	f := fixture(t)
	got, err := f.Select(nil)
	require.NoError(t, err)
	assert.Equal(t, names(f), names(got))
}

// A profile brings what its roots need, not just what it lists.
func TestSelectProfilePullsTheClosure(t *testing.T) {
	got, err := fixture(t).Select([]string{"storefront-only"})
	require.NoError(t, err)
	assert.Contains(t, names(got), "storefront", "what it listed")
	assert.Contains(t, names(got), "catalogue", "what storefront depends on")
	assert.Contains(t, names(got), "db", "and what that depends on")
	assert.NotContains(t, names(got), "mailer", "and nothing else")
}

// Any single name works as a target, so a repository gets the behaviour without
// declaring anything.
func TestSelectBareNameIsAProfileOfOne(t *testing.T) {
	got, err := fixture(t).Select([]string{"storefront"})
	require.NoError(t, err)
	assert.Contains(t, names(got), "storefront")
	assert.Contains(t, names(got), "catalogue")
	assert.NotContains(t, names(got), "mailer")
}

// A profile may include another.
func TestSelectProfilesCompose(t *testing.T) {
	got, err := fixture(t).Select([]string{"everything-but-mail"})
	require.NoError(t, err)
	assert.Contains(t, names(got), "storefront")
	assert.Contains(t, names(got), "seed")
	assert.NotContains(t, names(got), "mailer")
}

// Referring to something is depending on it: a service whose env carries
// another's address needs it up, whether or not depends_on says so.
func TestSelectFollowsReferencesNotJustDependsOn(t *testing.T) {
	f := fixture(t)
	// mailer reads the catalogue's address but does not declare it.
	for i, s := range f.Services {
		if s.Name == "mailer" {
			f.Services[i].DependsOn = nil
			f.Services[i].Env = map[string]string{"CATALOGUE_ADDR": "{{ catalogue.grpc }}"}
		}
	}
	got, err := f.Select([]string{"mailer"})
	require.NoError(t, err)
	assert.Contains(t, names(got), "catalogue", "pulled in by the reference alone")
}

// A profile sets not just what runs but how a dependency is wired: selecting it
// makes the chosen mode the dependency's default, so the run starts in it.
func TestSelectProfileSetsDependencyMode(t *testing.T) {
	f := fixture(t)

	// Without the profile, inventory starts on its manifest default.
	base, err := f.Select([]string{"storefront-only"})
	require.NoError(t, err)
	assert.Equal(t, "local", base.dependency("inventory").DefaultMode())

	// The staging profile runs the same things but starts inventory forwarded.
	staged, err := f.Select([]string{"storefront-staging"})
	require.NoError(t, err)
	assert.Contains(t, names(staged), "storefront", "it still runs the storefront")
	assert.Equal(t, "staging", staged.dependency("inventory").DefaultMode(),
		"the profile chose the mode the run starts in")

	// The override is on the selection, not the shared manifest.
	assert.Equal(t, "local", f.dependency("inventory").DefaultMode(),
		"the manifest default is untouched")
}

// A profile that names a mode a dependency does not have is caught at load.
func TestProfileModeValidation(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "devctl.yaml")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	for name, body := range map[string]string{
		"unknown dependency": "profiles:\n  - name: p\n    include: [a]\n    modes: {ghost: x}\nservices:\n  - name: a\n    cmd: x\n",
		"single source":      "dependencies:\n  - name: db\n    env: X\nprofiles:\n  - name: p\n    include: [a]\n    modes: {db: staging}\nservices:\n  - name: a\n    cmd: x\n",
		"unknown mode":       "dependencies:\n  - name: d\n    modes:\n      - {name: local, env: X}\nprofiles:\n  - name: p\n    include: [a]\n    modes: {d: nope}\nservices:\n  - name: a\n    cmd: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, body))
			assert.Error(t, err)
		})
	}
}

// An unknown target says what it could have been, since the answer is a list
// only the manifest has.
func TestSelectUnknownTargetLists(t *testing.T) {
	_, err := fixture(t).Select([]string{"nope"})
	require.ErrorContains(t, err, "unknown target")
	require.ErrorContains(t, err, "storefront-only")
	require.ErrorContains(t, err, "catalogue")
}
