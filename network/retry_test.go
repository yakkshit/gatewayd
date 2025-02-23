package network

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/gatewayd-io/gatewayd/config"
	"github.com/gatewayd-io/gatewayd/logging"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestRetry(t *testing.T) {
	logger := logging.NewLogger(context.Background(), logging.LoggerConfig{
		Output:            []config.LogOutput{config.Console},
		TimeFormat:        zerolog.TimeFormatUnix,
		ConsoleTimeFormat: time.RFC3339,
		Level:             zerolog.DebugLevel,
		NoColor:           true,
	})

	t.Run("DialTimeout", func(t *testing.T) {
		t.Run("nil", func(t *testing.T) {
			// Nil retry should just dial the connection once.
			var retry *Retry
			_, err := retry.Retry(nil)
			assert.Error(t, err)
			assert.ErrorContains(t, err, "callback is nil")
		})
		t.Run("retry without timeout", func(t *testing.T) {
			retry := NewRetry(0, 0, 0, false, logger)
			assert.Equal(t, 1, retry.Retries)
			assert.Equal(t, time.Duration(0), retry.Backoff)
			assert.Equal(t, float64(0), retry.BackoffMultiplier)
			assert.False(t, retry.DisableBackoffCaps)

			conn, err := retry.Retry(func() (any, error) {
				return net.Dial("tcp", "localhost:5432") //nolint: wrapcheck
			})
			assert.NoError(t, err)
			assert.NotNil(t, conn)
			assert.IsType(t, &net.TCPConn{}, conn)
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				tcpConn.Close()
			} else {
				t.Errorf("Unexpected connection type: %T", conn)
			}
		})
		t.Run("retry with timeout", func(t *testing.T) {
			retry := NewRetry(
				config.DefaultRetries,
				config.DefaultBackoff,
				config.DefaultBackoffMultiplier,
				config.DefaultDisableBackoffCaps,
				logger,
			)
			assert.Equal(t, config.DefaultRetries, retry.Retries)
			assert.Equal(t, config.DefaultBackoff, retry.Backoff)
			assert.Equal(t, config.DefaultBackoffMultiplier, retry.BackoffMultiplier)
			assert.False(t, retry.DisableBackoffCaps)

			conn, err := retry.Retry(func() (any, error) {
				return net.DialTimeout("tcp", "localhost:5432", config.DefaultDialTimeout) //nolint: wrapcheck
			})
			assert.NoError(t, err)
			assert.NotNil(t, conn)
			assert.IsType(t, &net.TCPConn{}, conn)
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				tcpConn.Close()
			} else {
				t.Errorf("Unexpected connection type: %T", conn)
			}
		})
	})
}
