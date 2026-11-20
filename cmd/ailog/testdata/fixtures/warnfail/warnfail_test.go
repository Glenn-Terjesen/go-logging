package warnfail

import (
	"testing"

	"github.com/entur/go-logging"
)

func TestWarnA(t *testing.T) {
	logging.Warn().Str("request_id", "req-a").Msg("upstream rejected credential A")
	t.Error("generic assertion A")
}

func TestWarnB(t *testing.T) {
	logging.Warn().Str("request_id", "req-b").Msg("upstream rejected credential B")
	t.Error("generic assertion B")
}
