// Package evals holds Factor's behavioural evaluations.
//
// The test suite around this one is very good at the half it covers: a race
// detector, a coverage gate, and a private X server driving real helpers
// against real pixels. All of it checks the deterministic parts — that a
// function given this input produces that output.
//
// The other half was untested. What actually steers Factor is its prompt: the
// system prompt, the operating rules restated as a conversation grows, the
// per-channel briefing, and the description on every tool. None of that is
// code with a return value, so none of it had a test, and a prompt edit
// shipped on judgement alone. That is the gap these fill.
//
// An eval is a case, not an assertion: an inbound message, a scripted model
// that decides what to do from what it was actually sent, and checks on the
// trajectory that followed. The provider is a fake and the tools are real, so
// no API key is needed and the whole suite runs in `make check` alongside
// everything else. The scripted model here is a stand-in for judgement, not a
// simulation of one — these evals ask whether the harness put the right things
// in front of a model and did the right thing with what came back, which is
// where the failures this project has actually had came from.
//
// The suite reports a pass rate rather than only failing, because a rate is
// the thing to watch across a prompt change: CI gates it, and a drop is the
// signal to look at what moved.
package evals
