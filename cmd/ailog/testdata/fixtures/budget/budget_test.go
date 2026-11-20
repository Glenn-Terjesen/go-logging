package budget

import (
	"fmt"
	"os"
	"testing"

	"github.com/entur/go-logging"
)

// Busy failures fill the diagnostic budget before the package crashes.
func TestBudget(t *testing.T) {
	for i := range 5 {
		t.Run(fmt.Sprint("sub", i), func(t *testing.T) {
			for j := range 8 {
				t.Logf("setup %d.%d", i, j)
				logging.Warn().Int("sub", i).Int("step", j).Msg("busy warning")
			}
			t.Error("busy failure")
		})
	}
}

func TestCrashLast(t *testing.T) {
	fmt.Fprintln(os.Stderr, "fatal cause: SPEC-CRASH-MUST-SHOW")
	os.Exit(1)
}
