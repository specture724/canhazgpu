package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRootReserveAccess(t *testing.T) {
	origEUID := currentEUID
	origLookup := targetUserLookup
	t.Cleanup(func() {
		currentEUID = origEUID
		targetUserLookup = origLookup
	})

	t.Run("rejects non-root user", func(t *testing.T) {
		currentEUID = func() int { return 1000 }
		targetUserLookup = func(string) error { return nil }

		_, err := validateRootReserveAccess("alice")
		require.Error(t, err)
		assert.ErrorContains(t, err, "only be run by root")
	})

	t.Run("rejects root without target user", func(t *testing.T) {
		currentEUID = func() int { return 0 }
		targetUserLookup = func(string) error { return nil }

		_, err := validateRootReserveAccess("   ")
		require.Error(t, err)
		assert.ErrorContains(t, err, "--user is required")
	})

	t.Run("rejects nonexistent target user", func(t *testing.T) {
		currentEUID = func() int { return 0 }
		targetUserLookup = func(username string) error {
			return errors.New("unknown user " + username)
		}

		_, err := validateRootReserveAccess("ghost")
		require.Error(t, err)
		assert.ErrorContains(t, err, `target user "ghost" does not exist`)
	})

	t.Run("allows root with valid target user", func(t *testing.T) {
		currentEUID = func() int { return 0 }
		var lookedUp string
		targetUserLookup = func(username string) error {
			lookedUp = username
			return nil
		}

		user, err := validateRootReserveAccess(" alice ")
		require.NoError(t, err)
		assert.Equal(t, "alice", user)
		assert.Equal(t, "alice", lookedUp)
	})
}

func TestReserveCommand_EnforcesRootAndTargetUser(t *testing.T) {
	origEUID := currentEUID
	origLookup := targetUserLookup
	origViper := *viper.GetViper()
	t.Cleanup(func() {
		currentEUID = origEUID
		targetUserLookup = origLookup
		*viper.GetViper() = origViper
	})

	t.Run("non-root fails immediately", func(t *testing.T) {
		viper.Reset()
		viper.Set("reserve.user", "alice")
		currentEUID = func() int { return 1000 }
		targetUserLookup = func(string) error { return nil }

		err := reserveCmd.RunE(reserveCmd, nil)
		require.Error(t, err)
		assert.ErrorContains(t, err, "only be run by root")
	})

	t.Run("root without --user fails immediately", func(t *testing.T) {
		viper.Reset()
		currentEUID = func() int { return 0 }
		targetUserLookup = func(string) error { return nil }

		err := reserveCmd.RunE(reserveCmd, nil)
		require.Error(t, err)
		assert.ErrorContains(t, err, "--user is required")
	})

	t.Run("root with nonexistent --user fails immediately", func(t *testing.T) {
		viper.Reset()
		viper.Set("reserve.user", "no-such-user")
		currentEUID = func() int { return 0 }
		targetUserLookup = func(string) error { return errors.New("not found") }

		err := reserveCmd.RunE(reserveCmd, nil)
		require.Error(t, err)
		assert.ErrorContains(t, err, `target user "no-such-user" does not exist`)
	})
}

func TestRunReserve_AssignsOwnershipToTargetUser(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}

	origConfig := config
	testCfg := &types.Config{
		RedisHost:       "localhost",
		RedisPort:       6379,
		RedisDB:         15,
		MemoryThreshold: types.MemoryThresholdMB,
	}
	client := redis_client.NewClient(testCfg)
	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Skipf("Redis not available for testing: %v", err)
	}
	require.NoError(t, client.FlushTestDB(ctx))
	require.NoError(t, client.SetGPUCount(ctx, 2))
	require.NoError(t, client.SetAvailableProvider(ctx, "fake"))
	config = testCfg

	t.Cleanup(func() {
		config = origConfig
		_ = client.FlushTestDB(ctx)
		_ = client.Close()
	})

	// Immediate reservation assigns both User and ActualUser to targetUser.
	err := runReserve(ctx, reserveOptions{
		gpuIDs:         []int{0},
		durationStr:    "1h",
		idleTimeoutStr: "15m",
		targetUser:     "alice",
		nonblock:       true,
		short:          true,
	})
	require.NoError(t, err)

	state0, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, "alice", state0.User)
	assert.Equal(t, "alice", state0.ActualUser)
	assert.Equal(t, types.ReservationTypeManual, state0.Type)

	// Immediate booking (--start now) creates and immediately activates for targetUser.
	err = runReserve(ctx, reserveOptions{
		gpuIDs:         []int{1},
		durationStr:    "1h",
		idleTimeoutStr: "15m",
		startStr:       "now",
		targetUser:     "bob",
		short:          true,
	})
	require.NoError(t, err)

	state1, err := client.GetGPUState(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "bob", state1.User)
	assert.Equal(t, "bob", state1.ActualUser)
	assert.Equal(t, types.ReservationTypeManual, state1.Type)
	assert.NotEmpty(t, state1.BookingID)

	// Future booking assigns both User and ActualUser to targetUser without activating yet.
	err = runReserve(ctx, reserveOptions{
		gpuIDs:         []int{1},
		durationStr:    "1h",
		idleTimeoutStr: "15m",
		startStr:       "+2h",
		targetUser:     "carol",
		short:          true,
	})
	require.NoError(t, err)

	bookings, err := client.GetAllBookings(ctx)
	require.NoError(t, err)
	var futureBooking *types.Booking
	for _, b := range bookings {
		if b.User == "carol" {
			futureBooking = b
			break
		}
	}
	require.NotNil(t, futureBooking)
	assert.Equal(t, "carol", futureBooking.User)
	assert.Equal(t, "carol", futureBooking.ActualUser)
	assert.Equal(t, types.BookingStatusPending, futureBooking.Status)
	assert.True(t, futureBooking.StartTime.ToTime().After(time.Now().Add(time.Hour)))
}
