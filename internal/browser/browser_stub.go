//go:build nobrowser

// Stub build: -tags nobrowser strips chromedp and the whole browser suite
// from the binary for the smallest boxes.
package browser

import (
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

func NewTools(_ config.BrowserConfig, _ string) ([]tools.Tool, func()) {
	return nil, func() {}
}
