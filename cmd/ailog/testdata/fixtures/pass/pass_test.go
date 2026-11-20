package pass

import (
	"testing"

	"github.com/entur/go-logging"
)

func TestPass(t *testing.T) {
	logging.Warn().Str("request_id", "req-1").Msg("warning in a passing test")
}

func TestPassSub(t *testing.T) {
	t.Run("one", func(t *testing.T) {})
	t.Run("two", func(t *testing.T) {})
}
