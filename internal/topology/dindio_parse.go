package topology

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// maxStatsBytes bounds how much of a stats response is decoded. A real sample
// is a few kilobytes; the cap only guards against a misbehaving endpoint.
const maxStatsBytes = 1 << 20

// errNoBlkioWrites means a stats sample carried no block-IO write counter at
// all, as opposed to a counter that reads zero: the cgroup has no io
// controller, or the daemon omits blkio for this runtime.
var errNoBlkioWrites = errors.New("stats sample has no blkio write counters")

// parseBlkioWriteBytes reads one stats sample (the JSON body of the one-shot
// stats endpoint) and returns the block-device bytes the container wrote over
// its life: the sum of every "write" entry in blkio_stats.io_service_bytes_recursive
// across devices.
//
// The entry shape differs by cgroup version. cgroup v1 reports Read, Write,
// Sync, Async, Total (capitalised, and Total/Sync/Async overlap Read/Write);
// cgroup v2 reports read and write (lower case) from io.stat. Only the write op
// is summed, matched case-insensitively, so neither shape double counts.
func parseBlkioWriteBytes(r io.Reader) (uint64, error) {
	var sample struct {
		BlkioStats container.BlkioStats `json:"blkio_stats"`
	}
	if err := json.NewDecoder(io.LimitReader(r, maxStatsBytes)).Decode(&sample); err != nil {
		return 0, fmt.Errorf("failed to decode stats sample: %w", err)
	}
	var (
		total uint64
		found bool
	)
	for _, e := range sample.BlkioStats.IoServiceBytesRecursive {
		if strings.EqualFold(e.Op, "write") {
			total += e.Value
			found = true
		}
	}
	if !found {
		return 0, errNoBlkioWrites
	}
	return total, nil
}

// imageProbeScript runs inside the sidecar and prints one record per line,
// parsed by parseImageProbe. It talks to the sidecar's own daemon over the
// shared socket path and reads only metadata: image sizes come from `docker
// image inspect`, never from walking the data root, which is slow on the
// disks this feature is meant to measure. It exits non-zero when the daemon
// does not answer. The content directory is only meaningful for the
// containerd image store (a classic store also has a containerd directory,
// but for runtime state only), so it is read only then.
const imageProbeScript = `export DOCKER_HOST=unix:///run/docker.sock
info=$(docker info --format '{{.Driver}} {{.DriverStatus}}') || exit 3
echo "driver ${info%% *}"
case "$info" in
*io.containerd.snapshotter*)
  echo "store containerd"
  kib=$(du -sk /var/lib/docker/containerd/daemon/io.containerd.content.v1.content 2>/dev/null | cut -f1)
  [ -z "$kib" ] || echo "content_kib $kib" ;;
*) echo "store classic" ;;
esac
ids=$(docker image ls -q --no-trunc | sort -u) || exit 4
[ -z "$ids" ] || docker image inspect --format 'image {{.ID}} {{.Size}}' $ids`

// imageProbe is what imageProbeScript reports about the sidecar's daemon.
type imageProbe struct {
	// Driver is the daemon's storage driver or snapshotter ("overlay2",
	// "overlayfs", "vfs").
	Driver string
	// Store is "containerd" (the containerd image store: unpacked snapshots
	// plus compressed blobs) or "classic" (a graph driver), "" if unreported.
	Store string
	// Images is the number of distinct image IDs the daemon holds.
	Images int
	// ImageBytes sums the `docker image inspect` Size of each distinct image.
	// A layer shared by several images is counted once per image.
	ImageBytes uint64
	// ContentBytes is the on-disk size of the containerd content store, the
	// compressed blobs pulls write. Only set when HasContent is true.
	ContentBytes uint64
	HasContent   bool
}

// parseImageProbe decodes imageProbeScript's output. Unknown lines are ignored
// so the script can grow; a malformed record is an error rather than a silently
// wrong number. Repeated image IDs are counted once.
func parseImageProbe(out string) (imageProbe, error) {
	var (
		p    imageProbe
		seen = map[string]bool{}
	)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if err := p.applyRecord(fields, seen); err != nil {
			return imageProbe{}, fmt.Errorf("malformed probe line %q: %w", line, err)
		}
	}
	if p.Driver == "" {
		return imageProbe{}, errors.New("probe output has no driver record")
	}
	return p, nil
}

// applyRecord folds one probe record (already split into fields) into p.
func (p *imageProbe) applyRecord(fields []string, seen map[string]bool) error {
	switch fields[0] {
	case "driver":
		if len(fields) != 2 {
			return errors.New("want 2 fields")
		}
		p.Driver = fields[1]
	case "store":
		if len(fields) != 2 {
			return errors.New("want 2 fields")
		}
		p.Store = fields[1]
	case "content_kib":
		kib, err := parseUintField(fields, 2)
		if err != nil {
			return err
		}
		p.ContentBytes, p.HasContent = kib*1024, true
	case "image":
		size, err := parseUintField(fields, 3)
		if err != nil {
			return err
		}
		if !seen[fields[1]] {
			seen[fields[1]] = true
			p.Images++
			p.ImageBytes += size
		}
	}
	return nil
}

// parseUintField parses the last of want fields as an unsigned integer.
func parseUintField(fields []string, want int) (uint64, error) {
	if len(fields) != want {
		return 0, fmt.Errorf("want %d fields", want)
	}
	return strconv.ParseUint(fields[want-1], 10, 64)
}

// repoFromEnv names the GitHub entity a runner registers against from its
// container environment (RUNNER_ORG/RUNNER_REPO/RUNNER_ENTERPRISE, as the
// provider sets them): "org/repo", "org" or "enterprises/name". It returns ""
// when none is set.
func repoFromEnv(env []string) string {
	vals := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vals[k] = v
		}
	}
	switch {
	case vals["RUNNER_ORG"] != "" && vals["RUNNER_REPO"] != "":
		return vals["RUNNER_ORG"] + "/" + vals["RUNNER_REPO"]
	case vals["RUNNER_ORG"] != "":
		return vals["RUNNER_ORG"]
	case vals["RUNNER_ENTERPRISE"] != "":
		return "enterprises/" + vals["RUNNER_ENTERPRISE"]
	default:
		return ""
	}
}
