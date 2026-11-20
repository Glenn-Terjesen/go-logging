package fatal

import (
	"testing"

	"github.com/entur/go-logging"
)

func TestFatal(t *testing.T) {
	logging.Fatal().Msg("fatal cause")
}
