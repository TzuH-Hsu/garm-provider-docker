package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// FakeClient is an in-memory Client for tests that need no Docker daemon. Where the
// real daemon's behavior would otherwise hide a bug, the fake follows it; the method
// docs say where.
type FakeClient struct {
	mu     sync.Mutex
	nextID int

	// containers and networks are keyed by ID and also resolve by name. Volumes are
	// keyed by name, which is their only identifier.
	containers map[string]*fakeContainer
	networks   map[string]*fakeNetwork
	volumes    map[string]*fakeVolume

	// PulledImages records every ImagePull ref, in call order.
	PulledImages []string

	// PresentImages is the set of refs ImageInspectWithRaw reports as local; any other
	// ref is NotFound.
	PresentImages map[string]bool

	// ImageLabels holds the config labels ImageInspectWithRaw reports for a ref.
	ImageLabels map[string]map[string]string

	// PullErr, when set, is returned by ImagePull.
	PullErr error

	// DefaultLogDriver is the log driver of a container created without
	// HostConfig.LogConfig.Type. "" means json-file; "none" makes logs unreadable.
	DefaultLogDriver string

	// LogsErr, when set, is returned by ContainerLogs.
	LogsErr error

	// ExecErr, when set, is returned by ExecStream.
	ExecErr error

	// CreateErr, when set, is returned by ContainerCreate. No container is recorded
	// unless CreateErrLeaksContainer is also true, which models a create that reports
	// an error although the container exists.
	CreateErr               error
	CreateErrLeaksContainer bool

	// CreateErrContainerName limits CreateErr to the container with exactly this name,
	// so a test can fail one container of an allocation whatever the creation order.
	CreateErrContainerName string

	// ExecExitCode is the exit code ExecStream reports. A non-zero value models a
	// command that ran and failed; nothing is written to the tmpfs.
	ExecExitCode int

	// StartErr, when set, is returned by ContainerStart.
	StartErr error

	// StartErrContainerName limits StartErr to the container with exactly this name.
	StartErrContainerName string

	// HostLacksIPv6 models a host whose kernel has no IPv6, so a container with a
	// net.ipv6.* sysctl is rejected with IPv6SysctlUnsupportedError. runc does this at
	// ContainerStart; IPv6SysctlFailsAtCreate moves it to ContainerCreate, which then
	// records nothing, for a daemon that validates the sysctl up front.
	HostLacksIPv6           bool
	IPv6SysctlFailsAtCreate bool

	// IPv6SysctlDenied models a host with IPv6 where the runtime may not write the
	// sysctl: the same rejection at the same point (IPv6SysctlDeniedError). It also
	// honors IPv6SysctlFailsAtCreate.
	IPv6SysctlDenied bool

	// InspectErr, when set, is returned by ContainerInspect.
	InspectErr error

	// RemoveErr, when set, is returned by ContainerRemove.
	RemoveErr error

	// NetworkCreateErr, when set, is returned by NetworkCreate. No network is recorded
	// unless NetworkCreateErrLeaks is also true.
	NetworkCreateErr      error
	NetworkCreateErrLeaks bool

	// NetworkRemoveErr, when set, is returned by NetworkRemove.
	NetworkRemoveErr error

	// VolumeCreateErr, when set, is returned by VolumeCreate.
	VolumeCreateErr error

	// VolumeRemoveErr, when set, is returned by VolumeRemove.
	VolumeRemoveErr error

	// Execs records every ExecStream call, in order.
	Execs []ExecRecord

	// OutputExecs records every ExecOutput call. It reports ExecOutputText and
	// ExecOutputExit, or ExecOutputErr when set. ExecOutputFunc, when set, answers
	// instead of both; it runs without the fake's lock, so it may block on its context
	// to model a stalled exec. StatsErr is returned by ContainerStatsOneShot.
	OutputExecs    []ExecRecord
	ExecOutputText string
	ExecOutputExit int
	ExecOutputErr  error
	ExecOutputFunc func(ctx context.Context, cmd []string) (string, int, error)
	StatsErr       error

	// Created records the shape of every ContainerCreate call, in order. Unlike the
	// live containers it survives removal, so a test can inspect a helper that removed
	// itself.
	Created []CreatedContainer

	// BatchExitCode is the exit status ContainerWait reports.
	BatchExitCode int

	// BatchWaitErr, when set, is sent on ContainerWait's error channel instead of a status.
	BatchWaitErr error

	// RemoveOrder records every successful removal, in order, as "container:<id>",
	// "volume:<name>" or "network:<id>".
	RemoveOrder []string

	// CreateHook, when set, runs at the start of every ContainerCreate, before the name
	// check, so a test can occupy a name in the window between another caller's
	// duplicate check and its create. This and the other hooks run without the fake's
	// lock, so they may call back into the fake; each must guard against re-entry itself.
	CreateHook func()

	// VolumeListHook, when set, runs at the start of every VolumeList, so a test can land
	// a concurrent change while a caller enumerates volumes.
	VolumeListHook func()

	// VolumeInspectHook, when set, runs at the start of every VolumeInspect with the
	// volume name, so a test can replace a volume between a caller's list and its
	// re-inspect.
	VolumeInspectHook func(name string)

	// VolumeInspectErrHook, when set, runs after VolumeInspectHook with the volume name.
	// A non-nil result is returned by VolumeInspect without touching the store, which
	// models a transient inspect failure on a volume that still exists.
	VolumeInspectErrHook func(name string) error

	// VolumeRemoveHook, when set, runs at the start of every VolumeRemove with the volume
	// name, so a test can change the store between a caller's list and its remove.
	VolumeRemoveHook func(name string)
}

// ExecRecord captures one ExecStream call for test assertions.
type ExecRecord struct {
	ContainerID string
	Cmd         []string
	Stdin       []byte
}

// CreatedContainer is the recorded shape of one ContainerCreate call.
type CreatedContainer struct {
	Name        string
	Labels      map[string]string
	Entrypoint  []string
	Cmd         []string
	Mounts      []mount.Mount
	NetworkMode container.NetworkMode
	Sysctls     map[string]string
}

type fakeContainer struct {
	id     string
	name   string
	image  string
	labels map[string]string
	env    []string

	// tmpfs is the credential tmpfs contents by file name. Only ExecStream writes it.
	tmpfs map[string]string

	// tmpfsMounts is HostConfig.Tmpfs (path to option string): the mount configuration,
	// as opposed to tmpfs, the delivered contents.
	tmpfsMounts map[string]string

	// sysctls is HostConfig.Sysctls, checked by ContainerStart.
	sysctls map[string]string

	// networkMode is HostConfig.NetworkMode.
	networkMode string

	// mounts is HostConfig.Mounts, reported by ContainerInspect under .Mounts.
	mounts []mount.Mount

	// cmd is Config.Cmd; privileged and runtime are the HostConfig fields of the same name.
	cmd        []string
	privileged bool
	runtime    string

	// groupAdd is HostConfig.GroupAdd.
	groupAdd []string

	// rawRuntime is the Runtime exactly as the create request set it. ContainerInspect
	// reports the daemon default for an empty one, as the real daemon does.
	rawRuntime string

	// state is the Docker state string: created, running, exited or dead.
	state     string
	oomKilled bool

	exitCode int

	// finishedAt is State.FinishedAt; zero until the container has stopped.
	finishedAt time.Time

	// tty decides whether ContainerLogs frames output with the stdcopy header (false) or
	// returns raw bytes (true).
	tty bool

	// logs is what the container printed. logNotify is closed and replaced whenever logs
	// or state change, which wakes ContainerLogs followers.
	logs      []fakeLogEntry
	logNotify chan struct{}

	// logDriver is HostConfig.LogConfig.Type; "none" makes ContainerLogs fail.
	logDriver string

	// processes is the command lines ContainerTop reports while the container runs.
	processes []string

	// statsBody is the JSON ContainerStatsOneShot returns; empty models a sample with no blkio counters.
	statsBody string
}

type fakeNetwork struct {
	id     string
	name   string
	labels map[string]string
}

type fakeVolume struct {
	name   string
	labels map[string]string
}

// NewFakeClient constructs an empty FakeClient.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		containers:    make(map[string]*fakeContainer),
		networks:      make(map[string]*fakeNetwork),
		volumes:       make(map[string]*fakeVolume),
		PresentImages: make(map[string]bool),
		ImageLabels:   make(map[string]map[string]string),
	}
}

var _ Client = (*FakeClient)(nil)

// find resolves a container by ID, then by name. Callers hold f.mu.
func (f *FakeClient) find(idOrName string) *fakeContainer {
	if c, ok := f.containers[idOrName]; ok {
		return c
	}
	return f.findByName(idOrName)
}

// findByName returns the container with exactly this name, or nil. Callers hold f.mu.
func (f *FakeClient) findByName(name string) *fakeContainer {
	for _, c := range f.containers {
		if c.name == name {
			return c
		}
	}
	return nil
}

// ImagePull records refStr and returns an empty reader. Unless PullErr is set, the
// image counts as present afterwards.
func (f *FakeClient) ImagePull(_ context.Context, refStr string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.PulledImages = append(f.PulledImages, refStr)
	if f.PullErr != nil {
		return nil, f.PullErr
	}
	f.PresentImages[refStr] = true
	return io.NopCloser(strings.NewReader("")), nil
}

// ImageInspectWithRaw reports refStr as present if it is in PresentImages, else
// NotFound. The ID is a "sha256:<hex>" derived from the ref, stable per ref and
// different across refs, like the real content-addressed ID. Config.Labels comes
// from ImageLabels.
func (f *FakeClient) ImageInspectWithRaw(_ context.Context, imageID string) (types.ImageInspect, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.PresentImages[imageID] {
		return types.ImageInspect{}, nil, notFoundf("image %s not found", imageID)
	}
	insp := types.ImageInspect{ID: fakeImageID(imageID)}
	if labels, ok := f.ImageLabels[imageID]; ok {
		insp.Config = &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Labels: maps.Clone(labels)}}
	}
	return insp, nil, nil
}

// fakeImageID returns a stable "sha256:<hex>" over ref, shaped like a real image ID.
func fakeImageID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ExecStream models the credential-delivery exec on a running container: it records
// the call and extracts the streamed tar into the container's tmpfs. A command other
// than the delivery `tar -x` is an error, a malformed archive exits with
// tarFailureExitCode like `tar -x`, and ExecExitCode overrides the outcome. Nothing
// else writes the tmpfs: a docker cp could not reach it (see Client.ExecStream).
func (f *FakeClient) ExecStream(_ context.Context, containerID string, cmd []string, stdin io.Reader) (int, error) {
	var data []byte
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return 0, err
		}
		data = b
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ExecErr != nil {
		return 0, f.ExecErr
	}
	c := f.find(containerID)
	if c == nil {
		return 0, notFoundf("container %s not found", containerID)
	}
	if c.state != "running" {
		return 0, fmt.Errorf("cannot exec in container %s: not running", containerID)
	}

	f.Execs = append(f.Execs, ExecRecord{
		ContainerID: c.id,
		Cmd:         append([]string(nil), cmd...),
		Stdin:       data,
	})

	// ExecExitCode models a command that ran and failed: report it, write nothing.
	if f.ExecExitCode != 0 {
		return f.ExecExitCode, nil
	}

	// Any other command is a caller bug; fail loudly rather than pretend to run it.
	if !isCredentialTarExtract(cmd) {
		return 0, fmt.Errorf("fake ExecStream only models `tar -x -C %s`, got %v", credentialTarTargetDir, cmd)
	}

	// A malformed archive makes real `tar -x` exit non-zero.
	if err := extractTarIntoTmpfs(c, data); err != nil {
		return tarFailureExitCode, nil
	}
	return 0, nil
}

// credentialTarTargetDir mirrors spec.CredentialDir, duplicated to keep this package
// free of a spec dependency. The provider's create tests run the real delivery
// command through this fake, so drift between the two fails them.
const credentialTarTargetDir = "/run/garm"

// tarFailureExitCode is the exit code reported for an archive that is not a valid tar.
const tarFailureExitCode = 2

// fakeDefaultRuntime is the runtime ContainerInspect reports for a container created
// without one.
const fakeDefaultRuntime = "runc"

// expectedCredentialTarExtractCmd is the argv of the provider's credential delivery command.
var expectedCredentialTarExtractCmd = []string{"tar", "-x", "-p", "-C", credentialTarTargetDir}

// isCredentialTarExtract reports whether cmd is exactly the delivery command. A
// looser match would accept argv shapes the provider never runs.
func isCredentialTarExtract(cmd []string) bool {
	return slices.Equal(cmd, expectedCredentialTarExtractCmd)
}

// extractTarIntoTmpfs records each entry of the tar in the container's tmpfs, and
// returns an error for a malformed archive.
func extractTarIntoTmpfs(c *fakeContainer, data []byte) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err // malformed archive
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil { //nolint:gosec // bounded test input
			return err
		}
		if c.tmpfs == nil {
			c.tmpfs = map[string]string{}
		}
		c.tmpfs[hdr.Name] = buf.String()
	}
}

// Tmpfs returns a copy of the container's tmpfs contents by file name, resolving
// containerID by ID or name. It returns nil for an unknown container.
func (f *FakeClient) Tmpfs(containerID string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.tmpfs))
	for k, v := range c.tmpfs {
		out[k] = v
	}
	return out
}

// TmpfsMounts returns a copy of the container's HostConfig.Tmpfs map (path to
// option string), or nil for an unknown container.
func (f *FakeClient) TmpfsMounts(containerID string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.tmpfsMounts))
	for k, v := range c.tmpfsMounts {
		out[k] = v
	}
	return out
}

// rejectUnsupportedTmpfsMount mirrors the daemon rejecting a Mounts long-syntax tmpfs
// that carries uid or gid options ("invalid option: uid"). The HostConfig.Tmpfs short
// syntax is not a Mounts entry and passes.
func rejectUnsupportedTmpfsMount(hostConfig *container.HostConfig) error {
	if hostConfig == nil {
		return nil
	}
	for _, m := range hostConfig.Mounts {
		if m.Type != mount.TypeTmpfs || m.TmpfsOptions == nil {
			continue
		}
		for _, opt := range m.TmpfsOptions.Options {
			if len(opt) == 0 {
				continue
			}
			switch opt[0] {
			case "uid", "gid":
				return fmt.Errorf("invalid mount config for type %q: invalid option: %s", "tmpfs", opt[0])
			}
		}
	}
	return nil
}

// ContainerCreate records a container in the created state. Like the daemon it
// rejects a long-syntax tmpfs with uid or gid options, a duplicate name (Conflict)
// and, with IPv6SysctlFailsAtCreate, an unsupported net.ipv6.* sysctl. It creates
// any missing named volume it references, unlabeled. CreateErr fails the call, and
// with CreateErrLeaksContainer the container is recorded anyway.
func (f *FakeClient) ContainerCreate(_ context.Context, cfg *container.Config, hostConfig *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	// Runs before the lock is taken so the hook can occupy a name this call then collides with.
	if f.CreateHook != nil {
		f.CreateHook()
	}

	// Rejected before anything is recorded or an injected error is consulted, as the
	// daemon rejects a malformed request outright.
	if err := rejectUnsupportedTmpfsMount(hostConfig); err != nil {
		return container.CreateResponse{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	createErr := f.CreateErr
	if f.CreateErrContainerName != "" && containerName != f.CreateErrContainerName {
		createErr = nil
	}

	if createErr != nil && !f.CreateErrLeaksContainer {
		return container.CreateResponse{}, createErr
	}
	if f.IPv6SysctlFailsAtCreate {
		if key := ipv6SysctlKey(hostConfig); key != "" {
			if err := f.ipv6SysctlErr(key); err != nil {
				return container.CreateResponse{}, err
			}
		}
	}

	// The daemon answers 409 Conflict for a name already in use.
	if createErr == nil && containerName != "" && f.findByName(containerName) != nil {
		return container.CreateResponse{}, conflictf("Conflict. The container name %q is already in use", "/"+containerName)
	}

	f.nextID++
	id := "fake-" + strconv.Itoa(f.nextID)

	name := containerName
	if name == "" {
		name = id
	}

	c := &fakeContainer{
		id:     id,
		name:   name,
		labels: map[string]string{},
		state:  "created",
	}
	if cfg != nil {
		c.image = cfg.Image
		c.env = append([]string(nil), cfg.Env...)
		c.cmd = append([]string(nil), cfg.Cmd...)
		c.tty = cfg.Tty
		for k, v := range cfg.Labels {
			c.labels[k] = v
		}
	}
	if hostConfig != nil && len(hostConfig.Tmpfs) > 0 {
		c.tmpfsMounts = make(map[string]string, len(hostConfig.Tmpfs))
		for k, v := range hostConfig.Tmpfs {
			c.tmpfsMounts[k] = v
		}
	}
	if hostConfig != nil {
		c.networkMode = string(hostConfig.NetworkMode)
		c.mounts = append([]mount.Mount(nil), hostConfig.Mounts...)
		c.privileged = hostConfig.Privileged
		c.runtime = hostConfig.Runtime
		c.rawRuntime = hostConfig.Runtime
		c.groupAdd = append([]string(nil), hostConfig.GroupAdd...)
		c.logDriver = hostConfig.LogConfig.Type
		c.sysctls = cloneLabels(hostConfig.Sysctls)
	}
	if c.logDriver == "" {
		c.logDriver = f.DefaultLogDriver
	}
	// A missing named volume is created unlabeled, as `docker create -v missing:/x`
	// does on the real daemon. A caller that expects its labeled cache volume must
	// notice the substitution (ADR-003, "A cache's identity is its label set, not its
	// name").
	for _, m := range c.mounts {
		if m.Type == mount.TypeVolume && m.Source != "" {
			if _, ok := f.volumes[m.Source]; !ok {
				f.volumes[m.Source] = &fakeVolume{name: m.Source, labels: map[string]string{}}
			}
		}
	}
	f.containers[id] = c

	rec := CreatedContainer{Name: name, Labels: cloneLabels(c.labels)}
	if cfg != nil {
		rec.Entrypoint = append([]string(nil), cfg.Entrypoint...)
		rec.Cmd = append([]string(nil), cfg.Cmd...)
	}
	if hostConfig != nil {
		rec.Mounts = append([]mount.Mount(nil), hostConfig.Mounts...)
		rec.NetworkMode = hostConfig.NetworkMode
		rec.Sysctls = cloneLabels(hostConfig.Sysctls)
	}
	f.Created = append(f.Created, rec)

	// Ambiguous failure: the container is recorded yet an error is reported.
	if createErr != nil {
		return container.CreateResponse{ID: id}, createErr
	}

	return container.CreateResponse{ID: id}, nil
}

// ContainerStart marks the container running. It fails when a prerequisite is gone: a
// user-defined network or named volume that no longer exists, a `container:<ref>`
// network mode whose <ref> is missing or not running, or a net.ipv6.* sysctl rejected
// per HostLacksIPv6 and IPv6SysctlDenied. A <ref> that is not running is a Conflict, as
// on the daemon. A missing <ref> is NotFound here; the daemon answers a 500 system error.
func (f *FakeClient) ContainerStart(_ context.Context, containerID string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if f.StartErr != nil && (f.StartErrContainerName == "" || (c != nil && c.name == f.StartErrContainerName)) {
		return f.StartErr
	}
	if c == nil {
		return notFoundf("container %s not found", containerID)
	}
	for k := range c.sysctls {
		if strings.HasPrefix(k, "net.ipv6.") {
			if err := f.ipv6SysctlErr(k); err != nil {
				return err
			}
		}
	}
	if isUserDefinedNetwork(c.networkMode) && f.findNetworkByName(c.networkMode) == nil && f.networks[c.networkMode] == nil {
		return fmt.Errorf("failed to start container %s: network %s not found", containerID, c.networkMode)
	}
	// A `container:<ref>` mode joins another container's network namespace. The daemon
	// accepts the create whatever <ref> is and fails the start.
	if ref, ok := joinedNetnsOwner(c.networkMode); ok {
		owner := f.find(ref)
		if owner == nil {
			return notFoundf("joining network namespace of container: No such container: %s", ref)
		}
		if owner.state != "running" {
			return conflictf("cannot join network namespace of a non running container: container %s is %s", owner.name, owner.state)
		}
	}
	for _, m := range c.mounts {
		if m.Type == mount.TypeVolume && m.Source != "" {
			if _, ok := f.volumes[m.Source]; !ok {
				return fmt.Errorf("failed to start container %s: volume %s not found", containerID, m.Source)
			}
		}
	}
	c.state = "running"
	return nil
}

// ContainerStop marks the container exited and stamps FinishedAt.
func (f *FakeClient) ContainerStop(_ context.Context, containerID string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.find(containerID)
	if c == nil {
		return notFoundf("container %s not found", containerID)
	}
	c.state = "exited"
	c.finishedAt = time.Now()
	c.wakeLogFollowers()
	return nil
}

// SetState forces a container's state (created, running, exited or dead) and its
// OOMKilled flag. Entering exited or dead stamps FinishedAt if it is unset.
func (f *FakeClient) SetState(containerID, state string, oomKilled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c, ok := f.containers[containerID]; ok {
		c.state = state
		c.oomKilled = oomKilled
		if (state == "exited" || state == "dead") && c.finishedAt.IsZero() {
			c.finishedAt = time.Now()
		}
		c.wakeLogFollowers()
	}
}

// SetFinishedAt sets a container's State.FinishedAt.
func (f *FakeClient) SetFinishedAt(containerID string, finishedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c, ok := f.containers[containerID]; ok {
		c.finishedAt = finishedAt
	}
}

// SetExitCode sets the State.ExitCode a container reports, so a test can model a
// runner that exited with a specific code. Unknown IDs are ignored.
func (f *FakeClient) SetExitCode(containerID string, exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c, ok := f.containers[containerID]; ok {
		c.exitCode = exitCode
	}
}

// ContainerInspect returns the recorded state of containerID.
func (f *FakeClient) ContainerInspect(_ context.Context, containerID string) (types.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.InspectErr != nil {
		return types.ContainerJSON{}, f.InspectErr
	}
	c := f.find(containerID)
	if c == nil {
		return types.ContainerJSON{}, notFoundf("container %s not found", containerID)
	}
	return c.toContainerJSON(), nil
}

// ContainerRemove deletes the container, found by ID or name.
func (f *FakeClient) ContainerRemove(_ context.Context, containerID string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	c := f.find(containerID)
	if c == nil {
		return notFoundf("container %s not found", containerID)
	}
	delete(f.containers, c.id)
	c.wakeLogFollowers()
	f.RemoveOrder = append(f.RemoveOrder, "container:"+c.id)
	return nil
}

// ContainerList returns the containers matching the "label" filter in options.Filters.
func (f *FakeClient) ContainerList(_ context.Context, options container.ListOptions) ([]types.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []types.Container
	for _, c := range f.containers {
		if !options.Filters.MatchKVList("label", c.labels) {
			continue
		}
		out = append(out, c.toContainerSummary())
	}
	return out, nil
}

// ContainerWait models a helper container running to completion: a created or
// running container becomes exited, and BatchExitCode is sent on the response
// channel. BatchWaitErr, or an unknown container, is sent on the error channel
// instead. Both channels are buffered, so the call never blocks.
func (f *FakeClient) ContainerWait(_ context.Context, containerID string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.BatchWaitErr != nil {
		errCh <- f.BatchWaitErr
		return statusCh, errCh
	}
	c := f.find(containerID)
	if c == nil {
		errCh <- notFoundf("container %s not found", containerID)
		return statusCh, errCh
	}
	if c.state == "running" || c.state == "created" {
		c.state = "exited"
		if c.finishedAt.IsZero() {
			c.finishedAt = time.Now()
		}
		c.wakeLogFollowers()
	}
	statusCh <- container.WaitResponse{StatusCode: int64(f.BatchExitCode)}
	return statusCh, errCh
}

// findNetwork resolves a network by ID, then by name. Callers hold f.mu.
func (f *FakeClient) findNetwork(idOrName string) *fakeNetwork {
	if n, ok := f.networks[idOrName]; ok {
		return n
	}
	return f.findNetworkByName(idOrName)
}

// findNetworkByName returns the network with exactly this name, or nil. Callers hold f.mu.
func (f *FakeClient) findNetworkByName(name string) *fakeNetwork {
	for _, n := range f.networks {
		if n.name == name {
			return n
		}
	}
	return nil
}

// NetworkCreate fails with Conflict when the name is already in use, as the real
// daemon does unconditionally since API 1.44.
func (f *FakeClient) NetworkCreate(_ context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.NetworkCreateErr != nil && !f.NetworkCreateErrLeaks {
		return network.CreateResponse{}, f.NetworkCreateErr
	}

	if f.findNetworkByName(name) != nil {
		return network.CreateResponse{}, conflictf("network with name %q already exists", name)
	}

	f.nextID++
	id := "fake-net-" + strconv.Itoa(f.nextID)
	f.networks[id] = &fakeNetwork{id: id, name: name, labels: cloneLabels(options.Labels)}

	// Ambiguous failure: the network is recorded yet an error is reported.
	if f.NetworkCreateErr != nil {
		return network.CreateResponse{ID: id}, f.NetworkCreateErr
	}
	return network.CreateResponse{ID: id}, nil
}

// NetworkRemove deletes a network, found by ID or name. It fails with Forbidden ("has
// active endpoints") while any container, even a stopped one, is attached. That is
// stricter than the daemon, which rejects the removal only while an attached container
// runs, and it makes a teardown that removes the network first fail here.
func (f *FakeClient) NetworkRemove(_ context.Context, networkID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.NetworkRemoveErr != nil {
		return f.NetworkRemoveErr
	}
	n := f.findNetwork(networkID)
	if n == nil {
		return notFoundf("network %s not found", networkID)
	}
	for _, c := range f.containers {
		if c.networkMode == n.name || c.networkMode == n.id {
			return errdefs.Forbidden(fmt.Errorf("error while removing network: network %s id %s has active endpoints", n.name, n.id))
		}
	}
	delete(f.networks, n.id)
	f.RemoveOrder = append(f.RemoveOrder, "network:"+n.id)
	return nil
}

// NetworkList returns the networks matching the "label" filter in options.Filters.
func (f *FakeClient) NetworkList(_ context.Context, options network.ListOptions) ([]network.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []network.Summary
	for _, n := range f.networks {
		if !options.Filters.MatchKVList("label", n.labels) {
			continue
		}
		out = append(out, network.Summary{ID: n.id, Name: n.name, Labels: cloneLabels(n.labels)})
	}
	return out, nil
}

// VolumeCreate is idempotent on a duplicate name, unlike NetworkCreate and
// ContainerCreate: as the real daemon does, it returns the existing volume with its
// original labels and discards the new ones. A caller that needs a fresh volume must
// detect the collision itself.
func (f *FakeClient) VolumeCreate(_ context.Context, options volume.CreateOptions) (volume.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.VolumeCreateErr != nil {
		return volume.Volume{}, f.VolumeCreateErr
	}

	name := options.Name
	if name == "" {
		f.nextID++
		name = "fake-vol-" + strconv.Itoa(f.nextID)
	}

	if existing, ok := f.volumes[name]; ok {
		// The original volume's labels win.
		return volume.Volume{Name: existing.name, Labels: cloneLabels(existing.labels)}, nil
	}

	v := &fakeVolume{name: name, labels: cloneLabels(options.Labels)}
	f.volumes[name] = v
	return volume.Volume{Name: v.name, Labels: cloneLabels(v.labels)}, nil
}

// VolumeInspect returns one volume with its labels, or NotFound. VolumeInspectHook
// runs first, then VolumeInspectErrHook, which can fail the call for a volume that
// exists.
func (f *FakeClient) VolumeInspect(_ context.Context, volumeID string) (volume.Volume, error) {
	if f.VolumeInspectHook != nil {
		f.VolumeInspectHook(volumeID)
	}
	if f.VolumeInspectErrHook != nil {
		if err := f.VolumeInspectErrHook(volumeID); err != nil {
			return volume.Volume{}, err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.volumes[volumeID]
	if !ok {
		return volume.Volume{}, notFoundf("volume %s not found", volumeID)
	}
	return volume.Volume{Name: v.name, Labels: cloneLabels(v.labels)}, nil
}

// VolumeRemove deletes a volume by name. Like the daemon it fails with Conflict
// while any container, running or stopped, references the volume. force is accepted
// for parity with the SDK and does not override that.
func (f *FakeClient) VolumeRemove(_ context.Context, volumeID string, _ bool) error {
	if f.VolumeRemoveHook != nil {
		f.VolumeRemoveHook(volumeID)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.VolumeRemoveErr != nil {
		return f.VolumeRemoveErr
	}
	if _, ok := f.volumes[volumeID]; !ok {
		return notFoundf("volume %s not found", volumeID)
	}
	for _, c := range f.containers {
		for _, m := range c.mounts {
			if m.Type == mount.TypeVolume && m.Source == volumeID {
				return conflictf("remove %s: volume is in use - [%s]", volumeID, c.id)
			}
		}
	}
	delete(f.volumes, volumeID)
	f.RemoveOrder = append(f.RemoveOrder, "volume:"+volumeID)
	return nil
}

// VolumeList returns the volumes matching the "label" filter in options.Filters.
func (f *FakeClient) VolumeList(_ context.Context, options volume.ListOptions) (volume.ListResponse, error) {
	if f.VolumeListHook != nil {
		f.VolumeListHook()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*volume.Volume
	for _, v := range f.volumes {
		if !options.Filters.MatchKVList("label", v.labels) {
			continue
		}
		out = append(out, &volume.Volume{Name: v.name, Labels: cloneLabels(v.labels)})
	}
	return volume.ListResponse{Volumes: out}, nil
}

func (c *fakeContainer) toContainerJSON() types.ContainerJSON {
	state := &types.ContainerState{
		Status:    c.state,
		Running:   c.state == "running",
		Dead:      c.state == "dead",
		OOMKilled: c.oomKilled,
		ExitCode:  c.exitCode,
	}
	// FinishedAt is empty until the container has finished; the daemon reports the zero time.
	if !c.finishedAt.IsZero() {
		state.FinishedAt = c.finishedAt.UTC().Format(time.RFC3339Nano)
	}
	base := &types.ContainerJSONBase{
		ID:    c.id,
		Name:  "/" + c.name,
		State: state,
		HostConfig: &container.HostConfig{
			Tmpfs:      cloneLabels(c.tmpfsMounts),
			Privileged: c.privileged,
			// An empty runtime is reported as the daemon default; see RawRuntime.
			Runtime:  normalizeRuntime(c.runtime),
			GroupAdd: append([]string(nil), c.groupAdd...),
		},
	}
	if c.networkMode != "" {
		base.HostConfig.NetworkMode = container.NetworkMode(c.networkMode)
	}

	cj := types.ContainerJSON{
		ContainerJSONBase: base,
		Config: &container.Config{
			Image:  c.image,
			Tty:    c.tty,
			Env:    append([]string(nil), c.env...),
			Cmd:    strslice.StrSlice(append([]string(nil), c.cmd...)),
			Labels: cloneLabels(c.labels),
		},
		Mounts: c.mountPoints(),
	}
	// A user-defined network is reported under NetworkSettings.Networks with a fixed
	// private IP; built-in modes are not.
	if isUserDefinedNetwork(c.networkMode) {
		cj.NetworkSettings = &types.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				c.networkMode: {IPAddress: "10.240.0.2"},
			},
		}
	}
	return cj
}

// mountPoints converts the recorded mounts to the .Mounts that ContainerInspect reports.
func (c *fakeContainer) mountPoints() []types.MountPoint {
	if len(c.mounts) == 0 {
		return nil
	}
	out := make([]types.MountPoint, 0, len(c.mounts))
	for _, m := range c.mounts {
		out = append(out, types.MountPoint{
			Type:        m.Type,
			Name:        m.Source, // for a named volume, Source is the volume name
			Destination: m.Target,
			// RW is false for a ReadOnly mount, as in the daemon's inspect output.
			RW: !m.ReadOnly,
		})
	}
	return out
}

// normalizeRuntime maps an empty runtime to the daemon default, as inspect output
// does. Any other runtime is reported verbatim.
func normalizeRuntime(runtime string) string {
	if runtime == "" {
		return fakeDefaultRuntime
	}
	return runtime
}

// RawRuntime returns the Runtime exactly as the container's create request set it,
// "" when none was requested. It returns "" for an unknown container.
func (f *FakeClient) RawRuntime(containerID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c := f.find(containerID); c != nil {
		return c.rawRuntime
	}
	return ""
}

// isUserDefinedNetwork reports whether mode names a user-defined network rather than
// a built-in mode or a joined namespace.
func isUserDefinedNetwork(mode string) bool {
	if _, ok := joinedNetnsOwner(mode); ok {
		return false
	}
	switch mode {
	case "", "bridge", "host", "none", "default", "container":
		return false
	default:
		return true
	}
}

// joinedNetnsOwner returns <ref> for a `container:<ref>` network mode, and false for
// any other mode.
func joinedNetnsOwner(mode string) (string, bool) {
	ref, ok := strings.CutPrefix(mode, "container:")
	if !ok || ref == "" {
		return "", false
	}
	return ref, true
}

func (c *fakeContainer) toContainerSummary() types.Container {
	return types.Container{
		ID:     c.id,
		Names:  []string{"/" + c.name},
		Image:  c.image,
		Labels: cloneLabels(c.labels),
		Status: c.state,
		State:  c.state,
	}
}

func cloneLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// notFoundf builds an error satisfying errdefs.IsNotFound, as the SDK returns for a
// daemon 404.
func notFoundf(format string, a ...any) error {
	return errdefs.NotFound(fmt.Errorf(format, a...))
}

// conflictf builds an error satisfying errdefs.IsConflict, as the SDK returns for a
// daemon 409.
func conflictf(format string, a ...any) error {
	return errdefs.Conflict(fmt.Errorf(format, a...))
}

// ipv6SysctlKey returns the first net.ipv6.* key in hostConfig.Sysctls, or "".
func ipv6SysctlKey(hostConfig *container.HostConfig) string {
	if hostConfig == nil {
		return ""
	}
	for k := range hostConfig.Sysctls {
		if strings.HasPrefix(k, "net.ipv6.") {
			return k
		}
	}
	return ""
}

// ipv6SysctlErr is the rejection this fake host gives a net.ipv6.* sysctl, or
// nil when it applies it (callers hold f.mu).
func (f *FakeClient) ipv6SysctlErr(key string) error {
	switch {
	case f.HostLacksIPv6:
		return IPv6SysctlUnsupportedError(key)
	case f.IPv6SysctlDenied:
		return IPv6SysctlDeniedError(key)
	}
	return nil
}

// IPv6SysctlUnsupportedError is the error a runc-based daemon returns from
// ContainerStart when the host kernel has no IPv6, so it cannot apply sysctl key.
// The text is what Docker 29.1.3 with runc 1.3.4 reported for
// net.ipv6.conf.all.disable_ipv6 on a kernel booted with ipv6.disable=1.
func IPv6SysctlUnsupportedError(key string) error {
	return runcSysctlError(key, "no such file or directory")
}

// IPv6SysctlDeniedError is the same runc rejection for a host that has IPv6 but does
// not let the sysctl be written ("permission denied"). The wording follows
// IPv6SysctlUnsupportedError; it was not observed on a live daemon.
func IPv6SysctlDeniedError(key string) error {
	return runcSysctlError(key, "permission denied")
}

func runcSysctlError(key, reason string) error {
	path := "proc/./sys/" + strings.ReplaceAll(key, ".", "/")
	return errdefs.System(fmt.Errorf("failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: open sysctl %s file: unsafe procfs detected: openat2 fsmount:fscontext:%s: %s", key, path, reason))
}
