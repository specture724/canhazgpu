package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/russellb/canhazgpu/internal/hostbridge"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	config     *types.Config
	configFile string
	rootCmd    = &cobra.Command{
		Use:   "canhazgpu",
		Short: "A GPU reservation tool for single host shared development systems",
		Long: `canhazgpu provides a simple reservation system that coordinates GPU access 
across multiple users and processes on a single machine, ensuring exclusive access 
to requested GPUs while automatically handling cleanup when jobs complete or crash.`,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
)

func init() {
	rootCmd.PersistentPreRunE = prepareHostBridge
	cobra.OnInitialize(initConfig)

	// Global flags
	rootCmd.PersistentFlags().String("host-socket", "", "Host guard Unix socket (auto-detected at /run/canhazgpu/host.sock; 'off' disables)")
	rootCmd.PersistentFlags().String("docker-owners", "", "Host JSON file mapping Docker container IDs to accounts (default: /etc/canhazgpu/docker-owners.json)")
	rootCmd.PersistentFlags().StringVar(&configFile, "config", "", "config file (default is $HOME/.canhazgpu.yaml)")
	rootCmd.PersistentFlags().String("redis-host", "localhost", "Redis host")
	rootCmd.PersistentFlags().Int("redis-port", 6379, "Redis port")
	rootCmd.PersistentFlags().Int("redis-db", 0, "Redis database")
	rootCmd.PersistentFlags().Int("memory-threshold", types.MemoryThresholdMB, "Memory threshold in MB to consider a GPU as 'in use' (default: 100)")
	rootCmd.PersistentFlags().String("booking-protection-window", utils.FormatDurationShort(types.DefaultBookingProtectionWindow),
		"How far ahead reservations without a fixed end time avoid GPUs needed by scheduled bookings")

	if err := viper.BindPFlag("redis.host", rootCmd.PersistentFlags().Lookup("redis-host")); err != nil {
		panic(fmt.Sprintf("Failed to bind redis-host flag: %v", err))
	}
	if err := viper.BindPFlag("redis.port", rootCmd.PersistentFlags().Lookup("redis-port")); err != nil {
		panic(fmt.Sprintf("Failed to bind redis-port flag: %v", err))
	}
	if err := viper.BindPFlag("redis.db", rootCmd.PersistentFlags().Lookup("redis-db")); err != nil {
		panic(fmt.Sprintf("Failed to bind redis-db flag: %v", err))
	}
	if err := viper.BindPFlag("memory.threshold", rootCmd.PersistentFlags().Lookup("memory-threshold")); err != nil {
		panic(fmt.Sprintf("Failed to bind memory-threshold flag: %v", err))
	}
	if err := viper.BindPFlag("booking.protection_window", rootCmd.PersistentFlags().Lookup("booking-protection-window")); err != nil {
		panic(fmt.Sprintf("Failed to bind booking-protection-window flag: %v", err))
	}

	// Set defaults
	viper.SetDefault("redis.host", "localhost")
	viper.SetDefault("redis.port", 6379)
	viper.SetDefault("redis.db", 0)
	viper.SetDefault("memory.threshold", types.MemoryThresholdMB)
	viper.SetDefault("booking.protection_window", utils.FormatDurationShort(types.DefaultBookingProtectionWindow))
}

func initConfig() {
	if configFile != "" {
		// Use config file from the flag
		viper.SetConfigFile(configFile)
	} else {
		// Find home directory
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not find home directory: %v\n", err)
		} else {
			// Search config in home directory with name ".canhazgpu" (without extension)
			viper.AddConfigPath(home)
			viper.AddConfigPath(".")
			viper.SetConfigType("yaml")
			viper.SetConfigName(".canhazgpu")
		}
	}

	// Enable reading from environment variables. The replacer is what makes
	// nested keys reachable: without it "memory.threshold" would look for
	// CANHAZGPU_MEMORY.THRESHOLD, which a shell cannot even export.
	viper.SetEnvPrefix("CANHAZGPU")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	viper.AutomaticEnv()

	// If a config file is found, read it in
	_ = viper.ReadInConfig()

	// Bind all flags to viper for automatic config file support
	bindAllFlags()

	config = &types.Config{
		HostSocket:      viper.GetString("host-socket"),
		RedisHost:       viper.GetString("redis.host"),
		RedisPort:       viper.GetInt("redis.port"),
		RedisDB:         viper.GetInt("redis.db"),
		MemoryThreshold: viper.GetInt("memory.threshold"),
		RemoteHosts:     viper.GetStringSlice("remote_hosts"),
	}

	// An unparseable window falls back to the default rather than disabling
	// booking protection altogether
	if window, err := utils.ParseDuration(viper.GetString("booking.protection_window")); err == nil && window > 0 {
		config.BookingProtectionWindow = window
	} else {
		config.BookingProtectionWindow = types.DefaultBookingProtectionWindow
	}
}

func Execute(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

func SetVersion(v string) {
	rootCmd.Version = v
}

func getConfig() *types.Config {
	if config == nil {
		initConfig()
	}
	return config
}

// bindAllFlags automatically binds all command flags to viper
// This allows config files to override default values for any flag
func bindAllFlags() {
	// Walk through all commands and bind their flags
	walkCommands(rootCmd, func(cmd *cobra.Command) {
		cmd.Flags().VisitAll(func(flag *pflag.Flag) {
			// Create viper key from command and flag name
			viperKey := flag.Name
			if cmd.Name() != "canhazgpu" { // Don't prefix root command flags
				viperKey = cmd.Name() + "." + flag.Name
			}

			// Bind flag to viper
			if err := viper.BindPFlag(viperKey, flag); err != nil {
				panic(fmt.Sprintf("Failed to bind flag %s: %v", viperKey, err))
			}
		})
	})
}

// walkCommands recursively walks through all commands
func walkCommands(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, child := range cmd.Commands() {
		walkCommands(child, fn)
	}
}

func getCurrentUser() string {
	if config != nil && config.HostUser != "" {
		return config.HostUser
	}
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	if user := os.Getenv("USERNAME"); user != "" {
		return user
	}
	return "unknown"
}

func prepareHostBridge(cmd *cobra.Command, args []string) error {
	cfg := getConfig()
	ownersPath := viper.GetString("docker-owners")
	if ownersPath == "" && cmd.Name() == "guard" {
		// Preserve existing guard configuration files after promoting the flag
		// to a global option that standalone status can also use.
		ownersPath = viper.GetString("guard.docker-owners")
	}
	owners, err := loadContainerOwners(ownersPath)
	if err != nil {
		return fmt.Errorf("Docker owner mappings: %w", err)
	}
	cfg.ContainerOwners = owners
	if cmd.Name() == "guard" {
		if cfg.HostSocket != "" && cfg.HostSocket != "off" {
			return fmt.Errorf("guard must run on the host; remove --host-socket / CANHAZGPU_HOST_SOCKET")
		}
		cfg.HostSocket = ""
		return nil
	}
	if cfg.HostSocket == "off" {
		cfg.HostSocket = ""
		return nil
	}
	if cfg.HostSocket == "" {
		// The mounted directory survives a guard restart even while its socket
		// is absent. Do not switch container clients to local root/PIDs then.
		if info, err := os.Stat(filepath.Dir(hostbridge.DefaultSocket)); err == nil && info.IsDir() {
			cfg.HostSocket = hostbridge.DefaultSocket
		}
	}
	if cfg.HostSocket == "" {
		return nil
	}
	identity, err := (hostbridge.Client{Socket: cfg.HostSocket}).Identity(cmd.Context())
	if err != nil {
		return err
	}
	cfg.HostPID, cfg.HostUser = identity.PID, identity.User
	cfg.RedisDB, cfg.MemoryThreshold = identity.RedisDB, identity.MemoryThreshold
	if cmd.Name() == "admin" && !identity.Admin {
		return fmt.Errorf("admin through the host bridge requires host root")
	}
	return nil
}

func loadContainerOwners(path string) (map[string]string, error) {
	if path != "" {
		return hostbridge.LoadOwners(path)
	}
	owners, err := hostbridge.LoadOwners("/etc/canhazgpu/docker-owners.json")
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	return owners, err
}
