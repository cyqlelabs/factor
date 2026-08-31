package cron

// jobs.json seen as what it is: one file that several factor processes read,
// change and write back whole. Every test here runs two services over one
// store, which is a gateway and a `factor chat` sharing a home.

import (
	"testing"
	"time"
)

// The defect these lock and reload fixes close: the scheduler used to decide
// from one read of the store and then save the whole file from that decision,
// so a reminder another process wrote in between was erased — after its user
// had been told it was scheduled.
func TestFiringAJobKeepsWhatAnotherProcessWroteMeanwhile(t *testing.T) {
	dir := t.TempDir()
	gateway, clock := serviceAt(t, dir, moment(t, beforeNine), nil, nil)
	if _, err := gateway.Add("0 9 * * *", "morning brief", "telegram", "77"); err != nil {
		t.Fatal(err)
	}

	// Another process schedules a reminder of its own.
	cli, _ := serviceAt(t, dir, moment(t, beforeNine), nil, nil)
	if _, err := cli.Add("0 10 * * *", "the kettle", "telegram", "77"); err != nil {
		t.Fatal(err)
	}

	// The gateway fires its own job, and saves the store doing it.
	clock.set(moment(t, justAfter))
	if due := gateway.dueJobs(); len(due) != 1 || due[0].Message != "morning brief" {
		t.Fatalf("due = %+v", due)
	}

	fresh, _ := serviceAt(t, dir, moment(t, justAfter), nil, nil)
	messages := map[string]bool{}
	for _, j := range fresh.List() {
		messages[j.Message] = true
	}
	if !messages["the kettle"] || !messages["morning brief"] {
		t.Fatalf("a reminder was erased by the other process's save: %+v", fresh.List())
	}
}

// The lock is what makes that hold between processes rather than only between
// goroutines: a cycle waits for whoever is mid-write.
func TestTheStoreLockKeepsTwoProcessesOutOfEachOthersWrites(t *testing.T) {
	dir := t.TempDir()
	holder, _ := serviceAt(t, dir, moment(t, beforeNine), nil, nil)
	other, _ := serviceAt(t, dir, moment(t, beforeNine), nil, nil)

	release := holder.lockStore()
	added := make(chan error, 1)
	go func() {
		_, err := other.Add("0 9 * * *", "waits its turn", "telegram", "77")
		added <- err
	}()

	select {
	case err := <-added:
		t.Fatalf("a write went ahead while another process held the store (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case err := <-added:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the write never resumed after the lock was released")
	}
}

// And a process that wedges holding the lock must not stop the schedule: the
// wait is bounded, and the cycle goes ahead rather than not at all.
func TestAWedgedLockDoesNotStopTheSchedule(t *testing.T) {
	previous := lockWait
	lockWait = 50 * time.Millisecond
	t.Cleanup(func() { lockWait = previous })

	dir := t.TempDir()
	holder, _ := serviceAt(t, dir, moment(t, beforeNine), nil, nil)
	other, _ := serviceAt(t, dir, moment(t, beforeNine), nil, nil)

	release := holder.lockStore()
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := other.Add("0 9 * * *", "goes ahead anyway", "telegram", "77")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a held lock stopped the scheduler for good")
	}
}
