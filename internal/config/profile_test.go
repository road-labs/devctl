package config

import (
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

// An unknown target says what it could have been, since the answer is a list
// only the manifest has.
func TestSelectUnknownTargetLists(t *testing.T) {
	_, err := fixture(t).Select([]string{"nope"})
	require.ErrorContains(t, err, "unknown target")
	require.ErrorContains(t, err, "storefront-only")
	require.ErrorContains(t, err, "catalogue")
}
