package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogsMaxSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"1024", 1024},
		{"512KB", 512 << 10},
		{"10MB", 10 << 20},
		{" 2 GB ", 2 << 30},
		{"10mb", 10 << 20},
	} {
		got, err := (&Logs{MaxSize: tc.in}).Bytes()
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

// A cap that cannot be read is an error, not a zero: a log that was meant to be
// capped and silently is not is a disk that fills up overnight.
func TestLogsMaxSizeRefusesNonsense(t *testing.T) {
	_, err := (&Logs{MaxSize: "ten megabytes"}).Bytes()
	require.Error(t, err)
	_, err = (&Logs{MaxSize: "10TB"}).Bytes()
	require.Error(t, err)
}

// No logs block means no files, and asking for the cap is still safe.
func TestLogsNilIsNoCap(t *testing.T) {
	var logs *Logs
	got, err := logs.Bytes()
	require.NoError(t, err)
	assert.Zero(t, got)
}

// The cap is checked when the manifest loads, so `-check` refuses it rather
// than the first start silently running without one.
func TestLoadRefusesAnUnreadableMaxSize(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/devctl.yaml"
	require.NoError(t, os.WriteFile(path, []byte(`
logs:
  dir: .devlogs
  max_size: ten megabytes
services:
  - name: svc
    cmd: "true"
`), 0o644))

	_, err := Load(path)
	require.ErrorContains(t, err, "max_size")
}
