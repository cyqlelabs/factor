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
	// minTotalMB is the memory a machine needs to hold Laya beside an
	// agent without swapping; the box this exists for has 3.5 GB.
	minTotalMB = 4096
)

// Engine is the model this backend runs, for the wiring that depends on
// it: the browser's decisions are Laya's alone.
func (b *Backend) Engine() string {
	if b == nil {
		return EngineLaya
	}
	return b.engine()
}

// engine resolves the configured engine, deciding "auto" the first time and
// keeping the answer: a model that changed underneath a running gateway
// would change every threshold the traces were calibrated against. "auto"
// reads what the machine is — the CPU's flags and the memory it was built
// with — rather than what is free at the moment, which a browser tab moves.
// A configured command names Laya's interpreter, so it pins "auto" to Laya.
func (b *Backend) engine() string {
	if e, ok := b.chosen.Load().(string); ok && e != "" {
		return e
	}
	e := b.cfg.Engine
	switch e {
	case EngineLaya, EngineStudent:
	default:
		e = EngineLaya
		if b.cfg.Command == "" {
			if !hasAVX2() {
				e = EngineStudent
				slog.Info("decisions run on the student model: the CPU has no AVX2, which Laya's int8 kernels need")
			} else if total, ok := totalMB(); ok && total < minTotalMB {
				e = EngineStudent
				slog.Info("decisions run on the student model: not enough memory for Laya beside an agent", "total_mb", total)
			}
		}
	}
	// Whoever decided first wins; the others read that answer.
	if b.chosen.CompareAndSwap(nil, e) {
		return e
	}
	return b.chosen.Load().(string)
}

// adopt takes the engine from a server that already answers on the port —
// the health probe says which model it is — so a student someone runs by
// hand is named, cached and thresholded as one.
func (b *Backend) adopt(backend string) {
	if backend == EngineStudent || backend == EngineLaya {
		b.chosen.CompareAndSwap(nil, backend)
	}
}

// studentCommand is the smrti binary that serves the student, from the
// resolver the memory engine's own supervisor uses to find it.
func (b *Backend) studentCommand(ctx context.Context) (string, error) {
	if b.cfg.Command != "" {
		return resolveInterpreter(b.cfg.Command)
	}
	if b.cfg.Student == nil {
		return "", fmt.Errorf("the student decision model is served by smrti, and this install runs no memory engine to serve it; decision.engine: laya runs Laya instead")
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
