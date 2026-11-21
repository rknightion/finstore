package testpg

import (
	"testing"
	"time"
)

// A loaded parallel run needs well over 15 s to start a cluster, and a helper command killed by its
// own context while pg_ctl is still waiting would turn a slow start into a failure with no
// explanation. The command bound outlasts the longest wait with room to spare.
func TestStartWaitsLongerThanALoadedMachineNeedsAndTheCommandOutlastsIt(t *testing.T) {
	if startTimeoutSeconds < 60 {
		t.Errorf("start waits %d s, want at least 60", startTimeoutSeconds)
	}
	for name, wait := range map[string]int{"start": startTimeoutSeconds, "stop": stopTimeoutSeconds} {
		if commandTimeout <= time.Duration(wait)*time.Second+30*time.Second {
			t.Errorf("a %s waits %d s but its command is bounded at %v", name, wait, commandTimeout)
		}
	}
}
