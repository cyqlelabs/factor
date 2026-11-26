package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// KeepTasks is how long a task stays on disk after it last changed. A caller
// that restarted reconciles by asking for its tasks, so they have to outlive
// a weekend with the machine off.
const KeepTasks = 7 * 24 * time.Hour

// restartReason is what a task that was running when this agent stopped says
// about itself. Resuming one is out of scope: the turn that was producing it
// lived in memory.
const restartReason = "this agent restarted while the task was running"

// Store keeps tasks on disk, one file each, and only ever shows a task to
// the agent that created it.
type Store struct {
	dir  string
	keep time.Duration
	now  func() time.Time

	mu sync.Mutex
}

type stored struct {
	Task    *a2a.Task `json:"task"`
	Version int64     `json:"version"`
	Owner   string    `json:"owner"`
	Updated time.Time `json:"updated"`
}

var _ taskstore.Store = (*Store)(nil)

// OpenStore opens the store in dir. Tasks older than keep are removed, and
// any task still marked as running is failed: whatever was running it died
// with the last process.
func OpenStore(dir string, keep time.Duration) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, keep: keep, now: time.Now}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	for _, rec := range s.allLocked() {
		if Terminal(rec.Task.Status.State) || rec.Task.Status.State == a2a.TaskStateUnspecified {
			continue
		}
		if err := s.setStatusLocked(rec, a2a.TaskStateFailed, restartReason); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// requester is the agent a call is made by, or "" for this process's own
// calls, which see every task.
func requester(ctx context.Context) string {
	if cc, ok := a2asrv.CallContextFrom(ctx); ok && cc.User != nil && cc.User.Authenticated {
		return cc.User.Name
	}
	return ""
}

func visible(ctx context.Context, rec *stored) bool {
	who := requester(ctx)
	return who == "" || who == rec.Owner
}

func (s *Store) path(id a2a.TaskID) (string, bool) {
	name := string(id)
	safe := func(r rune) bool {
		return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_'
	}
	if name == "" || len(name) > 64 || strings.ContainsFunc(name, func(r rune) bool { return !safe(r) }) {
		return "", false
	}
	return filepath.Join(s.dir, name+".json"), true
}

func (s *Store) readLocked(id a2a.TaskID) (*stored, error) {
	path, ok := s.path(id)
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	var rec stored
	if err := json.Unmarshal(data, &rec); err != nil || rec.Task == nil {
		return nil, fmt.Errorf("task %s is unreadable: %v", id, err)
	}
	return &rec, nil
}

func (s *Store) writeLocked(rec *stored) error {
	path, ok := s.path(rec.Task.ID)
	if !ok {
		return fmt.Errorf("task id %q cannot be stored", rec.Task.ID)
	}
	rec.Updated = s.now()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) allLocked() []*stored {
	entries, _ := os.ReadDir(s.dir)
	var out []*stored
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if rec, err := s.readLocked(a2a.TaskID(id)); err == nil {
			out = append(out, rec)
		}
	}
	return out
}

func (s *Store) pruneLocked() {
	cutoff := s.now().Add(-s.keep)
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
}

// Create implements taskstore.Store. The agent creating a task owns it.
func (s *Store) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.readLocked(task.ID); err == nil {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	s.pruneLocked()
	rec := &stored{Task: task, Version: 1, Owner: requester(ctx)}
	if err := s.writeLocked(rec); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return taskstore.TaskVersion(rec.Version), nil
}

// Update implements taskstore.Store with optimistic concurrency.
func (s *Store) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.readLocked(req.Task.ID)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	if !visible(ctx, rec) {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if req.PrevVersion != taskstore.TaskVersionMissing && taskstore.TaskVersion(rec.Version) != req.PrevVersion {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}
	rec.Task = req.Task
	rec.Version++
	if err := s.writeLocked(rec); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	return taskstore.TaskVersion(rec.Version), nil
}

// Get implements taskstore.Store. Another agent's task is not found.
func (s *Store) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.readLocked(id)
	if err != nil {
		return nil, err
	}
	if !visible(ctx, rec) {
		return nil, a2a.ErrTaskNotFound
	}
	return &taskstore.StoredTask{Task: rec.Task, Version: taskstore.TaskVersion(rec.Version), User: rec.Owner}, nil
}

// List implements taskstore.Store over the caller's own tasks, newest first.
// It does not page: a member keeps a week of tasks, which is one page.
func (s *Store) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var recs []*stored
	for _, rec := range s.allLocked() {
		if !visible(ctx, rec) {
			continue
		}
		if req.ContextID != "" && rec.Task.ContextID != req.ContextID {
			continue
		}
		if req.Status != a2a.TaskStateUnspecified && rec.Task.Status.State != req.Status {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Updated.After(recs[j].Updated) })
	resp := &a2a.ListTasksResponse{TotalSize: len(recs)}
	if req.PageSize > 0 && len(recs) > req.PageSize {
		recs = recs[:req.PageSize]
	}
	for _, rec := range recs {
		resp.Tasks = append(resp.Tasks, rec.Task)
	}
	resp.PageSize = len(resp.Tasks)
	return resp, nil
}

// setStatus writes a status straight onto a task nothing is executing — a
// question that went unanswered, a turn that ended while its caller was
// away. The next execution reads it like any other version.
func (s *Store) setStatus(id a2a.TaskID, state a2a.TaskState, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.readLocked(id)
	if err != nil {
		return err
	}
	if Terminal(rec.Task.Status.State) {
		return nil
	}
	return s.setStatusLocked(rec, state, text)
}

func (s *Store) setStatusLocked(rec *stored, state a2a.TaskState, text string) error {
	task := *rec.Task
	now := s.now()
	status := a2a.TaskStatus{State: state, Timestamp: &now}
	if text != "" {
		status.Message = a2a.NewMessageForTask(a2a.MessageRoleAgent, &task, a2a.NewTextPart(text))
	}
	task.Status = status
	if state == a2a.TaskStateCompleted && text != "" {
		task.Artifacts = append(task.Artifacts, &a2a.Artifact{ID: a2a.NewArtifactID(), Parts: a2a.ContentParts{a2a.NewTextPart(text)}})
	}
	rec.Task = &task
	rec.Version++
	return s.writeLocked(rec)
}
