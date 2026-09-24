package local

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
)

// Two models answer the same contract. Laya is the 300M multilingual
// checkpoint that answers any typed question; the student is smrti's
// 6-layer distillation of it, answering the fixed questions Factor and smrti
// ask. On a machine without AVX2 — where onnxruntime's int8 kernels fall to
// SSE2 paths and a 2011 dual core was measured at 30–48 s per Laya decision
// against 4 s deadlines — the student is the one that answers in time, at
// about a seventh of the memory. Everywhere else Laya's accuracy is worth
// its cost, and "auto" picks accordingly, once, from what the kernel reports.
const (
	EngineAuto    = "auto"
	EngineLaya    = "laya"
	EngineStudent = "student"
)

// engine resolves the configured engine, deciding "auto" the first time and
// keeping the answer: a model that changed underneath a running gateway
// would change every threshold the traces were calibrated against.
func (b *Backend) engine() string {
	if e, ok := b.chosen.Load().(string); ok && e != "" {
		return e
	}
	e := b.cfg.Engine
	switch e {
	case EngineLaya, EngineStudent:
	default:
		e = EngineLaya
		if !hasAVX2() {
			e = EngineStudent
			slog.Info("decisions run on the student model: the CPU has no AVX2, which Laya's int8 kernels need")
		} else if have, small := tooSmall(); small {
			e = EngineStudent
			slog.Info("decisions run on the student model: not enough memory for Laya", "available_mb", have)
		}
	}
	b.chosen.Store(e)
	return e
}

// studentCommand is the smrti binary that serves the student, from the
// resolver the memory engine's own supervisor uses to find it.
func (b *Backend) studentCommand(ctx context.Context) (string, error) {
	if b.cfg.Command != "" {
		return resolveInterpreter(b.cfg.Command)
	}
	if b.cfg.Student == nil {
		return "", fmt.Errorf("the student decision model needs smrti, and no smrti is configured here")
	}
	return b.cfg.Student(ctx)
}

// studentArgs is how smrti is told to serve the student on the decision
// port, holding the same share of the machine Laya would.
func (b *Backend) studentArgs() []string {
	return []string{
		"serve", "decisions",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(b.cfg.port()),
		"--threads", strconv.Itoa(computeThreads()),
	}
}
