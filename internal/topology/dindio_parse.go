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

// parseBlkioWriteBytes reads one sample from the one-shot stats endpoint and returns
// the block-device bytes the container wrote over its life, across devices. It returns
// errNoBlkioWrites when the sample has no write counter.
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
		// cgroup v1 reports Read, Write, Sync, Async and Total, which overlap; v2
		// reports read and write. Matching only the write op, in either case, double
		// counts neither.
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

// ioStatPath is the sidecar's own cgroup v2 io.stat as seen from inside it.
// On a cgroup v2 host Docker gives the sidecar a private cgroup namespace (the
// daemon default there, and one the DinD entrypoint relies on when it moves its
// processes into an init/ child), so /sys/fs/cgroup is the sidecar's cgroup.
// io.stat is recursive: it covers the entrypoint's init/ child and the nested
// daemon's docker/ subtree, so every container the job ran is counted.
const ioStatPath = "/sys/fs/cgroup/io.stat"

// ioStatScript prints the sidecar's cgroup io.stat for parseIOStat: an "io_stat"
// marker, then each line prefixed with "io ". An empty file (no device saw IO yet)
// prints the marker alone; a missing or unreadable one (a cgroup v1 host, no io
// controller, a runtime that virtualises cgroupfs) prints "io_stat_missing". It only
// reads a file, so it never waits on the sidecar's daemon.
const ioStatScript = `if [ -e ` + ioStatPath + ` ] && io=$(sed 's/^/io /' ` + ioStatPath + ` 2>/dev/null); then
  echo io_stat
  [ -z "$io" ] || printf '%s\n' "$io"
else
  echo io_stat_missing
fi`

// errNoIOStat means the sidecar's cgroup io.stat could not be read at all, as
// opposed to a file with no write counters, which is a valid zero.
var errNoIOStat = errors.New("cgroup io.stat not readable in the sidecar")

// deviceWrites is one device's line of io.stat: "major:minor" and its wbytes.
type deviceWrites struct {
	Device string
	Bytes  uint64
}

// ioStat is what ioStatScript reports.
type ioStat struct {
	// Written is the sum of wbytes over every device in io.stat. Devices are
	// not filtered: a swap device (zram, for example) also shows up, but its
	// major number is assigned at boot, so excluding it would be guesswork.
	// Summing every device also keeps the meaning of the stats endpoint
	// fallback, which sums every device too. Devices lists the per-device
	// figures so an operator can subtract a swap device.
	Written uint64
	Devices []deviceWrites
}

// byDevice formats Devices as "8:0=123,251:0=4".
func (s ioStat) byDevice() string {
	parts := make([]string, 0, len(s.Devices))
	for _, d := range s.Devices {
		parts = append(parts, d.Device+"="+strconv.FormatUint(d.Bytes, 10))
	}
	return strings.Join(parts, ",")
}

// parseIOStat reads the io.stat records in the output of ioStatScript and ignores every
// other line. It returns errNoIOStat when the script found no readable io.stat, and an
// error for a malformed line rather than a silently wrong number.
func parseIOStat(out string) (ioStat, error) {
	var (
		st      ioStat
		present bool
	)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "io_stat":
			present = true
		case "io":
			d, err := parseIOStatLine(fields[1:])
			if err != nil {
				return ioStat{}, fmt.Errorf("malformed io.stat line %q: %w", line, err)
			}
			st.Devices = append(st.Devices, d)
			st.Written += d.Bytes
		}
	}
	if !present {
		return ioStat{}, errNoIOStat
	}
	return st, nil
}

// parseIOStatLine parses one io.stat line, "MAJ:MIN rbytes=.. wbytes=.. ...",
// already split into fields. Keys other than wbytes are ignored (the kernel
// adds some when other io controllers are configured); a missing wbytes is an
// error.
func parseIOStatLine(fields []string) (deviceWrites, error) {
	if len(fields) == 0 {
		return deviceWrites{}, errors.New("no device")
	}
	major, minor, ok := strings.Cut(fields[0], ":")
	if !ok || !isDecimal(major) || !isDecimal(minor) {
		return deviceWrites{}, fmt.Errorf("device %q is not MAJ:MIN", fields[0])
	}
	for _, kv := range fields[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k != "wbytes" {
			continue
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return deviceWrites{}, fmt.Errorf("bad wbytes: %w", err)
		}
		return deviceWrites{Device: fields[0], Bytes: n}, nil
	}
	return deviceWrites{}, errors.New("no wbytes")
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// imageProbeScript runs inside the sidecar and prints one record per line for
// parseImageProbe. It reads only metadata from the sidecar's daemon: image sizes come
// from `docker image inspect`, never from walking the data root, which is slow on the
// disks this measures. It exits non-zero when the daemon does not answer or the image
// list or inspect fails. Each step's status is checked on its own, because a pipeline
// reports only its last command and would turn a failed list into a successful "no
// images". The content size is optional, omitted when `du` fails, and read only for the
// containerd image store: a classic store's containerd directory holds runtime state
// only.
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
ids=$(docker image ls -q --no-trunc) || exit 4
ids=$(printf '%s\n' "$ids" | sort -u) || exit 5
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
