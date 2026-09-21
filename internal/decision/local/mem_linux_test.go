//go:build linux

package local

import "testing"

// The floor is only worth having if the number behind it is real, so this
// reads the machine the test is running on rather than a fixture: a Linux box
// always has MemAvailable, and an answer outside these bounds means the parse
// is wrong rather than that the machine is unusual.
func TestAvailableMemoryIsReadFromTheMachine(t *testing.T) {
	have, ok := availableMB()
	if !ok {
		t.Fatal("MemAvailable could not be read on a Linux machine")
	}
	if have < 16 || have > 64*1024*1024 {
		t.Errorf("availableMB() = %d MB, which is not a plausible reading", have)
	}
}
