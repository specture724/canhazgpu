package cli

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadContainerOwnersExplicitFile(t *testing.T) {
	account, err := user.Current()
	require.NoError(t, err)
	if account.Uid == "0" {
		t.Skip("owner mapping requires a non-root account")
	}
	id := strings.Repeat("a", 64)
	path := filepath.Join(t.TempDir(), "owners.json")
	data, err := json.Marshal(map[string]string{id: account.Username})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	owners, err := loadContainerOwners(path)
	require.NoError(t, err)
	require.Equal(t, account.Username, owners[id])
	_, err = loadContainerOwners(path + ".missing")
	require.Error(t, err, "explicit missing files must not silently discard mappings")
	require.NoError(t, os.WriteFile(path, []byte("not JSON"), 0600))
	_, err = loadContainerOwners(path)
	require.Error(t, err)
}

func TestDockerOwnerFlagIsGlobal(t *testing.T) {
	require.NotNil(t, rootCmd.PersistentFlags().Lookup("docker-owners"))
	require.Nil(t, guardCmd.LocalNonPersistentFlags().Lookup("docker-owners"))
}
