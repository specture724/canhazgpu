package cli

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuardMaxTasksPerUserConfig(t *testing.T) {
	originalViper := *viper.GetViper()
	flag := guardCmd.Flags().Lookup("max-tasks-per-user")
	originalValue, originalChanged := flag.Value.String(), flag.Changed
	t.Cleanup(func() {
		require.NoError(t, flag.Value.Set(originalValue))
		flag.Changed = originalChanged
		*viper.GetViper() = originalViper
	})

	for _, tt := range []struct {
		name, config, env, flag string
		want                    int
		wantErr                 bool
	}{
		{name: "default", want: 4},
		{name: "config file", config: "2", want: 2},
		{name: "config disables", config: "0", want: 0},
		{name: "environment overrides config", config: "2", env: "3", want: 3},
		{name: "flag overrides environment and config", config: "2", env: "3", flag: "6", want: 6},
		{name: "zero flag overrides config", config: "2", flag: "0", want: 0},
		{name: "negative config", config: "-1", wantErr: true},
		{name: "negative flag", flag: "-1", wantErr: true},
		{name: "invalid config", config: "invalid", wantErr: true},
		{name: "fractional config", config: "1.5", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			bindAllFlags()
			require.NoError(t, flag.Value.Set(flag.DefValue))
			flag.Changed = false
			viper.SetConfigType("yaml")
			if tt.config != "" {
				require.NoError(t, viper.ReadConfig(strings.NewReader("guard:\n  max-tasks-per-user: "+tt.config)))
			}
			require.NoError(t, viper.BindEnv("guard.max-tasks-per-user", "CANHAZGPU_GUARD_MAX_TASKS_PER_USER"))
			t.Setenv("CANHAZGPU_GUARD_MAX_TASKS_PER_USER", tt.env)
			if tt.flag != "" {
				require.NoError(t, guardCmd.Flags().Set("max-tasks-per-user", tt.flag))
			}
			settings, err := guardSettingsFromConfig()
			if tt.wantErr {
				require.ErrorContains(t, err, "max-tasks-per-user")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, settings.MaxTasksPerUser)
		})
	}
}
