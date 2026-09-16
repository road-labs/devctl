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

// Targets is what shell completion offers: profiles and every single service,
// task and dependency, so `devctl <TAB>` lists them all.
func TestTargets(t *testing.T) {
	kind := map[string]string{}
	var names []string
	for _, tg := range fixture(t).Targets() {
		kind[tg.Name] = tg.Kind
		names = append(names, tg.Name)
	}
	assert.Equal(t, "profile", kind["storefront-only"])
	assert.Equal(t, "service", kind["catalogue"])
	assert.Equal(t, "dependency", kind["db"])
	assert.Equal(t, "task", kind["seed"])
	assert.IsIncreasing(t, names, "sorted, so completion lists them in order")
}

// A profile carries run-level env, applied to the run's services over their own.
func TestSelectProfileAppliesRunEnv(t *testing.T) {
	f := fixture(t)
	got, err := f.Select([]string{"storefront-staging"})
	require.NoError(t, err)

	var storefront *Service
	for i := range got.Services {
		if got.Services[i].Name == "storefront" {
			storefront = &got.Services[i]
		}
	}
	require.NotNil(t, storefront)
	assert.Equal(t, "staging", storefront.Env["SHOP_ENV"], "the profile's env lands on the run's services")
	assert.Equal(t, "{{ catalogue.http.url }}", storefront.Env["CATALOGUE_URL"], "its own env survives")

	for _, s := range f.Services {
		if s.Name == "storefront" {
			_, has := s.Env["SHOP_ENV"]
			assert.False(t, has, "the shared manifest is left untouched")
		}
	}
}

// The profile's env wins over a service's own value: it is the deliberate choice
// for this launch.
func TestProfileEnvWinsOverServiceEnv(t *testing.T) {
	body := "profiles:\n  - name: p\n    include: [a]\n    env: {LEVEL: run}\nservices:\n  - name: a\n    cmd: x\n    env: {LEVEL: service}\n"
	path := filepath.Join(t.TempDir(), "devctl.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	f, err := Load(path)
	require.NoError(t, err)

	got, err := f.Select([]string{"p"})
	require.NoError(t, err)
	assert.Equal(t, "run", got.Services[0].Env["LEVEL"], "the run's choice beats the service default")
}

// A profile's autostart is the definitive set for its run: it turns things on,
// and turns off anything that autostarts by default but is not listed.
func TestProfileAutostartOverridesTheRun(t *testing.T) {
	body := "profiles:\n  - name: p\n    include: [a, b]\n    autostart: [a]\nservices:\n  - name: a\n    cmd: x\n  - name: b\n    cmd: y\n    autostart: true\n"
	path := filepath.Join(t.TempDir(), "devctl.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	f, err := Load(path)
	require.NoError(t, err)

	svc := func(file *File, name string) Service {
		for _, s := range file.Services {
			if s.Name == name {
				return s
			}
		}
		t.Fatalf("%s missing", name)
		return Service{}
	}

	base, err := f.Select(nil)
	require.NoError(t, err)
	assert.True(t, svc(base, "b").Autostart, "its own flag holds without a profile")
	assert.False(t, svc(base, "a").Autostart)

	run, err := f.Select([]string{"p"})
	require.NoError(t, err)
	assert.True(t, svc(run, "a").Autostart, "listed, so it starts")
	assert.False(t, svc(run, "b").Autostart, "not listed, so it does not, even though it autostarts by default")
}

// A profile that autostarts something that is not a service or dependency is
// caught at load.
func TestProfileAutostartValidation(t *testing.T) {
	body := "profiles:\n  - name: p\n    include: [a]\n    autostart: [ghost]\nservices:\n  - name: a\n    cmd: x\n"
	path := filepath.Join(t.TempDir(), "devctl.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	_, err := Load(path)
	assert.Error(t, err)
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
