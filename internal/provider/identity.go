package provider

import (
	"net/http"

	"github.com/cyqlelabs/factor/internal/version"
)

// appName and appURL are how Factor introduces itself to a provider.
// OpenRouter reads HTTP-Referer and X-Title to name the app on its
// dashboard and rankings; everything else sees the User-Agent.
const (
	appName = "Factor"
	appURL  = "https://github.com/cyqlelabs/factor"
)

// Identify stamps every LLM request with the app's name, so the
// provider's logs and usage views attribute the traffic to Factor
// rather than to an anonymous Go client.
func Identify(h http.Header) {
	h.Set("User-Agent", "factor/"+version.Version+" (+"+appURL+")")
	h.Set("HTTP-Referer", appURL)
	h.Set("X-Title", appName)
}
