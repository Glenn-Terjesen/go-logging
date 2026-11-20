package huge

import (
	"strings"
	"testing"

	"github.com/entur/go-logging"
)

// The message follows a body larger than ailog's 1 MiB line cap.
func TestHuge(t *testing.T) {
	logging.Warn().Str("body", strings.Repeat("x", 1<<20+100)).Msg("CAUSE-ONLY-AT-END")
	logging.Warn().Msg("NEXT-EVENT")
	t.Error("huge failure")
}
