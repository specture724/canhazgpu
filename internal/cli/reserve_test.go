package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequireSudoAuthentication(t *testing.T) {
	original := sudoValidator
	t.Cleanup(func() { sudoValidator = original })

	t.Run("allows a validated user", func(t *testing.T) {
		called := false
		sudoValidator = func(context.Context) error {
			called = true
			return nil
		}

		assert.NoError(t, requireSudoAuthentication(context.Background()))
		assert.True(t, called)
	})

	t.Run("rejects an unvalidated user", func(t *testing.T) {
		sudoValidator = func(context.Context) error {
			return errors.New("authentication failed")
		}

		err := requireSudoAuthentication(context.Background())
		assert.Error(t, err)
		assert.ErrorContains(t, err, "sudo authentication is required")
	})
}

func TestReserveCommand_RequiresSudoAuthenticationBeforeReservation(t *testing.T) {
	original := sudoValidator
	t.Cleanup(func() { sudoValidator = original })

	sudoValidator = func(context.Context) error {
		return errors.New("authentication failed")
	}

	err := reserveCmd.RunE(reserveCmd, nil)
	assert.Error(t, err)
	assert.ErrorContains(t, err, "sudo authentication is required")
}
