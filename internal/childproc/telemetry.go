package childproc

// NoTelemetry is every switch that keeps something Factor runs from reporting
// on the machine it runs on. Each child gets all of them, whatever it is: the
// list is cheaper to apply everywhere than to keep in step with what each
// component happens to import this release, and a switch nothing reads costs
// nothing.
//
//   - ORT_DISABLE_TELEMETRY: onnxruntime posts the OS build, CPU model,
//     memory, a persistent device id and the interpreter path, which holds
//     the user's name, while it initializes — before any code could turn it
//     off (microsoft/onnxruntime#25573). The memory engine, the decision
//     model and both speech engines run on it.
//   - HF_HUB_DISABLE_TELEMETRY, DO_NOT_TRACK: huggingface_hub, which fetches
//     their models, reports library versions and what it downloaded.
//   - OTEL_SDK_DISABLED, PATTER_OTEL_ENABLED: OpenTelemetry export, which the
//     voice shell offers and several dependencies carry.
//   - CAMOFOX_CRASH_REPORT_ENABLED, SENTRY_DSN: the browser engine's crash and
//     error reports. A DSN in the user's own environment would otherwise send
//     its errors to that project.
//   - HOMEBREW_NO_ANALYTICS: Homebrew's install analytics, for the packages
//     Factor installs through it.
var NoTelemetry = []string{
	"ORT_DISABLE_TELEMETRY=1",
	"HF_HUB_DISABLE_TELEMETRY=1",
	"DO_NOT_TRACK=1",
	"OTEL_SDK_DISABLED=true",
	"PATTER_OTEL_ENABLED=0",
	"CAMOFOX_CRASH_REPORT_ENABLED=false",
	"SENTRY_DSN=",
	"HOMEBREW_NO_ANALYTICS=1",
}

// Quiet returns env with every NoTelemetry switch set. They go last, and
// os/exec keeps the last value of a repeated key, so a value the environment
// already held is overridden rather than trusted.
func Quiet(env []string) []string {
	return append(append([]string{}, env...), NoTelemetry...)
}
