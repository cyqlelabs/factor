//go:build nobrowser

// Stub build: -tags nobrowser strips chromedp and the whole browser suite
// from the binary for the smallest boxes.
package browser

import (
	"context"
	"errors"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/decision"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

func NewTools(_ config.BrowserConfig, _ string, _ *tools.PathGuard) ([]tools.Tool, func()) {
	return nil, func() {}
}

// Session, TextWriter and the executor exist in name only in this build.
type Session struct{}

type TextWriter interface {
	Chat(ctx context.Context, req *provider.Request) (*provider.Response, error)
}

func NewSuite(_ config.BrowserConfig, _ string, _ *tools.PathGuard) (*Session, []tools.Tool, func()) {
	return nil, nil, func() {}
}

// NewRunTool has no engine to run on; nil tells the caller to register nothing.
func NewRunTool(*Session, *decision.Decider, TextWriter) tools.Tool { return nil }

// Progress matches the real signature so the wizard compiles either way.
type Progress func(format string, args ...any)

var errStripped = errors.New("this build was made with -tags nobrowser: the browser suite is not included")

func Available() bool { return false }

func FindBrowserBinary(string) (string, error) { return "", errStripped }

func Verify(context.Context, config.BrowserConfig) error { return errStripped }

func EnsureEngine(context.Context, string, Progress) (string, bool, error) {
	return "", false, errStripped
}

func EnsureCamofox(context.Context, string, Progress) (string, bool, error) {
	return "", false, errStripped
}

func ProvisionInBackground(context.Context, config.BrowserConfig, string) {}
