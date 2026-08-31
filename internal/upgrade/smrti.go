package upgrade

// smrti ships as a container image and as a Python package, and this file
// keeps whichever one this machine runs current — a Factor that upgrades itself
// and leaves its memory engine two versions behind is only half current.
//
// A container is a registry lookup and a swap: pull the published image, wait
// for the graph to fall quiet, then recreate the container from the spec the
// running one carries — same volumes, same environment, same ports — so the
// memory on disk and the way it was configured both survive it. The engine is
// found by the port it answers on rather than by a name in the config: the
// container publishing Factor's memory port is, by definition, the engine
// Factor talks to.
//
// Everything else is the package half, in smrti_pkg.go: PyPI says what is
// published, the installer that made the install upgrades it, and the engine is
// restarted so the new code is actually loaded. An engine on another machine
// belongs to whoever runs it, and says so instead of pretending.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
)

// smrtiRepo is the published image, as <registry>/<repo>.
const smrtiRepo = "cyqlelabs/smrti"

// Seams: the whole path is testable without a registry or a docker daemon.
var (
	registryBase = "https://ghcr.io"
	dockerLook   = func() error { _, err := exec.LookPath("docker"); return err }
	dockerCmd    = func(ctx context.Context, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("docker %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}

	// Pacing, so a test does not sit through a real restart.
	smrtiIdleWait   = 3 * time.Minute
	smrtiIdlePoll   = time.Second
	smrtiHealthWait = 2 * time.Minute
	smrtiHealthPoll = 2 * time.Second
	// How long the engine gets to finish what it is doing on stop. smrti runs
	// its reflection passes on its own schedule, and SIGKILL mid-pass throws
	// that epoch away.
	smrtiStopTimeout = "30"
)

// Smrti keeps the memory engine current.
type Smrti struct {
	cfg  config.MemoryConfig
	idle func() bool
}

// NewSmrti builds the upgrader for the engine cfg points at. idle reports
// whether the graph is quiet enough to restart; a nil idle never waits.
func NewSmrti(cfg config.MemoryConfig, idle func() bool) *Smrti {
	return &Smrti{cfg: cfg, idle: idle}
}

// How the engine runs here, which is what decides how it is upgraded.
const (
	ModeContainer = "container"
	ModePackage   = "package"
)

// SmrtiRelease is the newest published smrti next to what is running.
type SmrtiRelease struct {
	Mode      string // container | package
	Running   string // what is installed here; empty when nothing can say
	Version   string // the newest version published for this mode
	Image     string // container: the image reference to run
	Container string // container: the container that would be replaced
	Path      string // package: the executable that would be upgraded
}

// Newer reports whether the published smrti is ahead of the running one. An
// install whose version nothing here can read counts as older: it may well be
// current, but the only way to find out is to install and see.
func (r SmrtiRelease) Newer() bool { return Newer(r.Running, r.Version) }

// RunningVersion is what to call the running engine in a sentence.
func (r SmrtiRelease) RunningVersion() string {
	if r.Running == "" {
		return "an unreadable version"
	}
	return r.Running
}

// Source names where the newest version was published, so a message says which
// half of smrti it is talking about.
func (r SmrtiRelease) Source() string {
	if r.Mode == ModePackage {
		return "published release"
	}
	return "published image"
}

// Check reports what is published against what is running here, in whichever
// way the engine runs. The container is looked for first: a docker that is
// broken rather than absent is reported rather than fallen past, since the
// engine it holds is the one that answers and a package upgraded instead of it
// would change nothing.
func (s *Smrti) Check(ctx context.Context) (SmrtiRelease, error) {
	c, err := s.container(ctx)
	switch {
	case err == nil:
		latest, err := latestSmrtiTag(ctx)
		if err != nil {
			return SmrtiRelease{}, err
		}
		return SmrtiRelease{
			Mode:      ModeContainer,
			Running:   imageTag(c.Config.Image),
			Version:   latest,
			Image:     smrtiImage(latest),
			Container: c.name(),
		}, nil
	case !errors.Is(err, errNoContainer):
		return SmrtiRelease{}, err
	}
	return s.checkPackage(ctx, err)
}

// Apply installs rel and restarts the engine into it, in place. It returns one
// clause saying what became of the engine, because that is the half of the
// outcome the caller cannot infer: an image swap ends with the engine
// answering, a package upgrade may end with code that only loads on the next
// start.
func (s *Smrti) Apply(ctx context.Context, rel SmrtiRelease, progress Progress) (string, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	if rel.Mode == ModePackage {
		return s.applyPackage(ctx, rel, progress)
	}
	// The image comes down first so the only downtime is the swap, the swap
	// waits for the graph to be idle, and a container that does not come back
	// healthy is rolled back to the one it replaced.
	c, err := s.container(ctx)
	if err != nil {
		return "", err
	}
	spec, err := runArgs(ctx, c, rel.Image)
	if err != nil {
		return "", err
	}
	progress("pulling %s", rel.Image)
	if _, err := dockerCmd(ctx, "pull", rel.Image); err != nil {
		return "", err
	}
	if err := s.waitIdle(ctx, progress); err != nil {
		return "", err
	}
	if err := s.swap(ctx, c.name(), rel, spec, progress); err != nil {
		return "", err
	}
	return "the engine is answering again", nil
}

// swap replaces the running container with one built from spec. The old one is
// renamed rather than removed: it is the rollback, and it still holds the
// image, environment and mounts that were working a moment ago.
func (s *Smrti) swap(ctx context.Context, name string, rel SmrtiRelease, spec []string, progress Progress) error {
	backup := name + "-" + strings.TrimPrefix(rel.Running, "v")
	if backup == name || rel.Running == "" {
		backup = name + "-previous"
	}
	_, _ = dockerCmd(ctx, "rm", "-f", backup) // an earlier swap may have left one

	progress("stopping %s", name)
	if _, err := dockerCmd(ctx, "stop", "--time", smrtiStopTimeout, name); err != nil {
		return err
	}
	if _, err := dockerCmd(ctx, "rename", name, backup); err != nil {
		_, _ = dockerCmd(ctx, "start", name)
		return err
	}

	progress("starting %s on %s", name, rel.Image)
	if _, err := dockerCmd(ctx, append([]string{"run"}, spec...)...); err != nil {
		return s.rollback(ctx, name, backup, progress, err)
	}
	progress("waiting for the memory engine to answer")
	if err := s.waitHealthy(ctx); err != nil {
		return s.rollback(ctx, name, backup, progress, err)
	}
	pruneBackups(ctx, name, backup)
	return nil
}

// pruneBackups leaves exactly one rollback behind: the container this swap just
// displaced. Every upgrade otherwise strands another stopped container holding
// another copy of a 700MB image. Only names this mechanism produces are
// touched — <engine>-<version> and <engine>-previous — so a container that
// merely shares the prefix is left alone.
func pruneBackups(ctx context.Context, name, keep string) {
	out, err := dockerCmd(ctx, "ps", "-a", "--format", "{{.Names}}")
	if err != nil {
		return
	}
	for _, other := range strings.Fields(out) {
		if other == keep || !strings.HasPrefix(other, name+"-") {
			continue
		}
		suffix := strings.TrimPrefix(other, name+"-")
		if _, isVersion := parseVersion(suffix); !isVersion && suffix != "previous" {
			continue
		}
		_, _ = dockerCmd(ctx, "rm", "-f", other)
	}
}

// rollback puts the previous container back under its own name. Its own
// failures are folded into the message: memory is not something to leave down
// silently, and the reader needs the two commands that finish the job by hand.
func (s *Smrti) rollback(ctx context.Context, name, backup string, progress Progress, cause error) error {
	progress("rolling back to %s", backup)
	_, _ = dockerCmd(ctx, "rm", "-f", name) // a container that failed to start still exists
	if _, err := dockerCmd(ctx, "rename", backup, name); err != nil {
		return fmt.Errorf("%w — the previous engine is still there as %s: docker rename %s %s && docker start %s",
			cause, backup, backup, name, name)
	}
	if _, err := dockerCmd(ctx, "start", name); err != nil {
		return fmt.Errorf("%w — and the previous engine did not come back up: %v", cause, err)
	}
	return fmt.Errorf("%w (rolled back to the engine that was running)", cause)
}

// waitIdle blocks until nothing is reading or writing the graph, which is the
// one moment a restart costs nothing. An engine that stays busy is left alone.
func (s *Smrti) waitIdle(ctx context.Context, progress Progress) error {
	if s.idle == nil || s.idle() {
		return nil
	}
	progress("waiting for the memory engine to go quiet")
	deadline := time.Now().Add(smrtiIdleWait)
	for {
		if !sleepCtx(ctx, smrtiIdlePoll) {
			return ctx.Err()
		}
		if s.idle() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the memory engine has been busy for %s; nothing was changed", smrtiIdleWait)
		}
	}
}

// waitHealthy blocks until the new container answers, so a swap only counts as
// done once memory works again.
func (s *Smrti) waitHealthy(ctx context.Context) error {
	client := memory.NewClient(s.cfg.BaseURL(), s.cfg.APIKey, "")
	deadline := time.Now().Add(smrtiHealthWait)
	var last error
	for {
		if last = client.CheckHealth(ctx); last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the new memory engine did not answer within %s: %w", smrtiHealthWait, last)
		}
		if !sleepCtx(ctx, smrtiHealthPoll) {
			return ctx.Err()
		}
	}
}

// Update is the whole engine half of `factor upgrade`: report what is
// published, and install it unless the caller only asked to look.
func (s *Smrti) Update(ctx context.Context, checkOnly bool, out Progress) error {
	if out == nil {
		out = func(string, ...any) {}
	}
	rel, err := s.Check(ctx)
	if err != nil {
		return err
	}
	switch {
	case !rel.Newer():
		out("smrti %s is the newest %s.", rel.RunningVersion(), rel.Source())
	case checkOnly:
		out("smrti %s is available — the engine here runs %s.", rel.Version, rel.RunningVersion())
	default:
		note, err := s.Apply(ctx, rel, out)
		if err != nil {
			return err
		}
		out("upgraded smrti %s to %s — %s", rel.RunningVersion(), rel.Version, note)
	}
	return nil
}

// Watch polls the registry for a newer engine image and reports each one once.
// Like the release watcher, it only ever reports: installing is asked for.
func (s *Smrti) Watch(ctx context.Context, every time.Duration, notify func(SmrtiRelease)) {
	told := ""
	watchLoop(ctx, every, func() {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rel, err := s.Check(callCtx)
		cancel()
		if err != nil {
			slogDebug("smrti image check failed", err)
			return
		}
		if rel.Version == told || !rel.Newer() {
			return
		}
		told = rel.Version
		notify(rel)
	})
}

// container is the docker inspect view of the engine, cut down to what
// recreating it needs.
type container struct {
	Name   string // "/smrti"
	Image  string // the resolved image ID, which is always inspectable
	Config struct {
		Image  string   // the reference it was started from, e.g. smrti:0.9.0
		Env    []string // image environment and -e flags, merged
		Cmd    []string
		User   string
		Labels map[string]string
	}
	HostConfig struct {
		Binds         []string
		NetworkMode   string
		PortBindings  map[string][]portBinding
		RestartPolicy struct{ Name string }
	}
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string
}

func (c container) name() string { return strings.TrimPrefix(c.Name, "/") }

// ErrNotManaged says there is no smrti here for Factor to upgrade — the engine
// runs on another machine, or nothing runs at all. It is not a failure: it is
// the answer, and callers that were not asked about the engine specifically
// stay quiet about it.
var ErrNotManaged = errors.New("no smrti here for factor to upgrade")

// errNoContainer says only that the engine is not a container, which is where
// the package half takes over.
var errNoContainer = errors.New("smrti does not run in a container here")

// container finds the running container that publishes the memory port.
func (s *Smrti) container(ctx context.Context) (container, error) {
	if err := dockerLook(); err != nil {
		return container{}, fmt.Errorf("%w (docker is not installed)", errNoContainer)
	}
	ids, err := dockerCmd(ctx, "ps", "--format", "{{.ID}}")
	if err != nil {
		return container{}, err
	}
	running := strings.Fields(ids)
	if len(running) == 0 {
		return container{}, s.noContainer()
	}
	out, err := dockerCmd(ctx, append([]string{"inspect"}, running...)...)
	if err != nil {
		return container{}, err
	}
	var found []container
	if err := json.Unmarshal([]byte(out), &found); err != nil {
		return container{}, fmt.Errorf("reading what docker is running: %w", err)
	}
	port := strconv.Itoa(s.cfg.Port)
	for _, c := range found {
		for _, bindings := range c.HostConfig.PortBindings {
			for _, b := range bindings {
				if b.HostPort == port {
					return c, nil
				}
			}
		}
	}
	return container{}, s.noContainer()
}

func (s *Smrti) noContainer() error {
	return fmt.Errorf("%w (nothing docker runs publishes port %d)", errNoContainer, s.cfg.Port)
}

// runArgs turns a running container into the `docker run` that recreates it on
// image. Everything the container carries beyond its image is copied over;
// everything the image itself supplied is left to the new image, which may
// have changed it — 0.9 moved `smrti` from the command into the entrypoint, so
// a blindly copied command would have run `smrti smrti serve rest`.
func runArgs(ctx context.Context, c container, image string) ([]string, error) {
	base, err := imageDefaults(ctx, c.Image)
	if err != nil {
		return nil, err
	}
	args := []string{"-d", "--name", c.name()}
	if p := c.HostConfig.RestartPolicy.Name; p != "" && p != "no" {
		args = append(args, "--restart", p)
	}
	if n := c.HostConfig.NetworkMode; n != "" && n != "default" && n != "bridge" {
		args = append(args, "--network", n)
	}
	if c.Config.User != "" && c.Config.User != base.User {
		args = append(args, "--user", c.Config.User)
	}
	for _, b := range c.HostConfig.Binds {
		args = append(args, "-v", b)
	}
	for _, p := range portSpecs(c.HostConfig.PortBindings) {
		args = append(args, "-p", p)
	}
	for name, value := range c.Config.Labels {
		args = append(args, "--label", name+"="+value)
	}
	for _, e := range addedEnv(c.Config.Env, base.Env) {
		args = append(args, "-e", e)
	}
	args = append(args, image)
	if !sameStrings(c.Config.Cmd, base.Cmd) {
		args = append(args, c.Config.Cmd...)
	}
	return args, nil
}

type imageSpec struct {
	Env  []string
	Cmd  []string
	User string
}

// imageDefaults reads what the image itself contributes, so the copy can leave
// it out. The container's resolved image ID is used rather than the tag it was
// started from: a tag can be moved to another image, an ID cannot.
func imageDefaults(ctx context.Context, image string) (imageSpec, error) {
	out, err := dockerCmd(ctx, "image", "inspect", image, "--format", "{{json .Config}}")
	if err != nil {
		return imageSpec{}, err
	}
	var spec imageSpec
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &spec); err != nil {
		return imageSpec{}, fmt.Errorf("reading the environment of image %s: %w", image, err)
	}
	return spec, nil
}

// addedEnv keeps the variables the container was given on top of its image.
// A value identical to the image's is dropped: it was never a choice, and
// pinning it would override whatever the new image chose instead.
func addedEnv(containerEnv, imageEnv []string) []string {
	fromImage := make(map[string]bool, len(imageEnv))
	for _, e := range imageEnv {
		fromImage[e] = true
	}
	var added []string
	for _, e := range containerEnv {
		if !fromImage[e] {
			added = append(added, e)
		}
	}
	return added
}

// portSpecs renders the published ports back into -p arguments, in a stable
// order (docker hands them over as a map).
func portSpecs(bindings map[string][]portBinding) []string {
	containerPorts := make([]string, 0, len(bindings))
	for p := range bindings {
		containerPorts = append(containerPorts, p)
	}
	sort.Strings(containerPorts)
	var specs []string
	for _, cp := range containerPorts {
		for _, b := range bindings[cp] {
			spec := b.HostPort + ":" + cp
			if b.HostIP != "" {
				spec = b.HostIP + ":" + spec
			}
			specs = append(specs, spec)
		}
	}
	return specs
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// smrtiImage is the image reference for a published tag.
func smrtiImage(tag string) string {
	return strings.TrimPrefix(strings.TrimPrefix(registryBase, "https://"), "http://") + "/" + smrtiRepo + ":" + tag
}

// imageTag is the version a container was started from — the tag of its image
// reference. An untagged or digest-pinned image has no version to compare, and
// reads as one, which Newer treats as older than anything published.
func imageTag(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

// latestSmrtiTag returns the newest version the registry publishes. Only fully
// qualified tags count: "latest" and the floating "0.9" say nothing about which
// build they point at today.
func latestSmrtiTag(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := registryToken(ctx)
	if err != nil {
		return "", err
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := getJSON(ctx, registryBase+"/v2/"+smrtiRepo+"/tags/list", token, &body); err != nil {
		return "", fmt.Errorf("looking up the published smrti images: %w", err)
	}
	best := ""
	for _, tag := range body.Tags {
		// Three numbers and nothing else: "latest" and the floating "0.9" do
		// not say which build they point at, and a "0.9.1-rc1" parses to the
		// release it precedes — installing a candidate nobody asked for.
		if strings.Count(strings.TrimPrefix(tag, "v"), ".") != 2 || strings.ContainsAny(tag, "-+") {
			continue
		}
		if _, ok := parseVersion(tag); !ok {
			continue
		}
		if best == "" || Newer(best, tag) {
			best = tag
		}
	}
	if best == "" {
		return "", fmt.Errorf("the smrti image registry publishes no versioned tag")
	}
	return best, nil
}

// registryToken fetches the anonymous pull token the registry hands out for a
// public image. A registry that wants no token at all is fine too.
func registryToken(ctx context.Context) (string, error) {
	var body struct {
		Token string `json:"token"`
	}
	url := registryBase + "/token?scope=repository:" + smrtiRepo + ":pull&service=" +
		strings.TrimPrefix(strings.TrimPrefix(registryBase, "https://"), "http://")
	if err := getJSON(ctx, url, "", &body); err != nil {
		return "", fmt.Errorf("authenticating to the smrti image registry: %w", err)
	}
	return body.Token, nil
}

func getJSON(ctx context.Context, url, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// sleepCtx waits out d and reports whether it got there before ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
