//go:build unit

package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLoadManagedUpdateSocketFromEnvironment(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("UPDATE_MANAGED_SOCKET", "/run/sub2api-car-updater/control.sock")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "/run/sub2api-car-updater/control.sock", cfg.Update.ManagedSocket)
}

func TestLoadManagedUpdateRejectsRelativeSocket(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("UPDATE_MANAGED_SOCKET", "relative.sock")
	_, err := Load()
	require.ErrorContains(t, err, "update.managed_socket must be an absolute Unix socket path")
}
