package cron

import (
	"path/filepath"
	"testing"
)

// breakStore points the service at an unwritable path so save() fails.
func breakStore(s *Service) { s.path = filepath.Join("/nonexistent-dir-for-tests", "jobs.json") }

func TestAddRollsBackWhenSaveFails(t *testing.T) {
	s := newService(t, nil, nil)
	breakStore(s)

	if _, err := s.Add("*/5 * * * *", "doomed", "cli", "1"); err == nil {
		t.Fatal("Add reported success despite a save failure")
	}
	if jobs := s.List(); len(jobs) != 0 {
		t.Errorf("failed Add left the job in memory: %+v", jobs)
	}

	// the sequence number is reusable, so ids do not skip
	s.path = filepath.Join(t.TempDir(), "jobs.json")
	job, err := s.Add("*/5 * * * *", "real", "cli", "1")
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "cron-1" {
		t.Errorf("id after a rolled-back Add = %q, want cron-1", job.ID)
	}
}

func TestRemoveAndSetEnabledRollBackWhenSaveFails(t *testing.T) {
	s := newService(t, nil, nil)
	job, err := s.Add("*/5 * * * *", "keep me", "cli", "1")
	if err != nil {
		t.Fatal(err)
	}
	breakStore(s)

	if err := s.Remove(job.ID); err == nil {
		t.Fatal("Remove reported success despite a save failure")
	}
	if jobs := s.List(); len(jobs) != 1 {
		t.Errorf("failed Remove dropped a job that is still on disk: %+v", jobs)
	}

	if err := s.SetEnabled(job.ID, false); err == nil {
		t.Fatal("SetEnabled reported success despite a save failure")
	}
	if jobs := s.List(); !jobs[0].Enabled {
		t.Error("failed SetEnabled left the flag flipped in memory only")
	}
}
