package topology

import (
	"errors"
	"strings"
	"testing"
)

func TestParseBlkioWriteBytes(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    uint64
		wantErr error
		errSub  string
	}{
		{
			name: "cgroup v2 lower-case ops summed across devices",
			body: `{"blkio_stats":{"io_service_bytes_recursive":[
				{"major":254,"minor":0,"op":"read","value":1000},
				{"major":254,"minor":0,"op":"write","value":300},
				{"major":254,"minor":16,"op":"read","value":50},
				{"major":254,"minor":16,"op":"write","value":200}]}}`,
			want: 500,
		},
		{
			name: "cgroup v1 capitalised ops: Total, Sync and Async do not double count",
			body: `{"blkio_stats":{"io_service_bytes_recursive":[
				{"major":8,"minor":0,"op":"Read","value":10},
				{"major":8,"minor":0,"op":"Write","value":700},
				{"major":8,"minor":0,"op":"Sync","value":710},
				{"major":8,"minor":0,"op":"Async","value":0},
				{"major":8,"minor":0,"op":"Total","value":710}]}}`,
			want: 700,
		},
		{
			name: "a zero write counter is a measurement, not an absence",
			body: `{"blkio_stats":{"io_service_bytes_recursive":[{"major":8,"minor":0,"op":"write","value":0}]}}`,
			want: 0,
		},
		{
			name: "unrelated fields in the sample are ignored",
			body: `{"read":"2026-11-02T00:00:00Z","cpu_stats":{"online_cpus":4},"blkio_stats":{"io_service_bytes_recursive":[{"major":8,"minor":0,"op":"write","value":42}]}}`,
			want: 42,
		},
		{
			name:    "null counters (no io controller) report absence",
			body:    `{"blkio_stats":{"io_service_bytes_recursive":null}}`,
			wantErr: errNoBlkioWrites,
		},
		{
			name:    "read-only entries report absence",
			body:    `{"blkio_stats":{"io_service_bytes_recursive":[{"major":8,"minor":0,"op":"read","value":9}]}}`,
			wantErr: errNoBlkioWrites,
		},
		{name: "empty object reports absence", body: `{}`, wantErr: errNoBlkioWrites},
		{name: "empty body is a decode error", body: ``, errSub: "failed to decode stats sample"},
		{name: "malformed JSON is a decode error", body: `{"blkio_stats":`, errSub: "failed to decode stats sample"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBlkioWriteBytes(strings.NewReader(tc.body))
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.errSub != "":
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.errSub)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			case got != tc.want:
				t.Fatalf("written = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseIOStat(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		want       uint64
		wantDevice string
		wantErr    error
		errSub     string
	}{
		{
			name: "wbytes summed over every device, swap included",
			out: "io_stat\n" +
				"io 8:0 rbytes=1024 wbytes=5539061760 rios=10 wios=200 dbytes=0 dios=0\n" +
				"io 251:0 rbytes=4096 wbytes=8192 rios=1 wios=2 dbytes=0 dios=0\n",
			want:       5539069952,
			wantDevice: "8:0=5539061760,251:0=8192",
		},
		{
			name:       "extra keys from other io controllers are ignored",
			out:        "io_stat\nio 259:0 rbytes=1 wbytes=77 rios=1 wios=1 dbytes=0 dios=0 cost.vrate=100.00 cost.usage=5\n",
			want:       77,
			wantDevice: "259:0=77",
		},
		{
			name: "a present but empty io.stat is a valid zero",
			out:  "io_stat\n",
			want: 0,
		},
		{
			name:       "image probe lines and stray stderr are ignored",
			out:        "io_stat\nio 8:0 rbytes=0 wbytes=9 rios=0 wios=1 dbytes=0 dios=0\ndriver overlay2\nimage sha256:aa 5\nCannot connect\n",
			want:       9,
			wantDevice: "8:0=9",
		},
		{name: "missing io.stat reports absence", out: "io_stat_missing\ndriver overlay2\n", wantErr: errNoIOStat},
		{name: "no marker at all reports absence", out: "driver overlay2\n", wantErr: errNoIOStat},
		{name: "empty output reports absence", out: "", wantErr: errNoIOStat},
		{name: "non-numeric wbytes", out: "io_stat\nio 8:0 rbytes=1 wbytes=lots\n", errSub: "bad wbytes"},
		{name: "negative wbytes", out: "io_stat\nio 8:0 wbytes=-1\n", errSub: "bad wbytes"},
		{name: "line without wbytes", out: "io_stat\nio 8:0 rbytes=1 rios=1\n", errSub: "no wbytes"},
		{name: "device that is not MAJ:MIN", out: "io_stat\nio sda wbytes=1\n", errSub: "not MAJ:MIN"},
		{name: "device with a non-numeric minor", out: "io_stat\nio 8:x wbytes=1\n", errSub: "not MAJ:MIN"},
		{name: "record with no device", out: "io_stat\nio\n", errSub: "no device"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseIOStat(tc.out)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.errSub != "":
				if err == nil || !strings.Contains(err.Error(), tc.errSub) || !strings.Contains(err.Error(), "malformed io.stat line") {
					t.Fatalf("err = %v, want a malformed line error containing %q", err, tc.errSub)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			default:
				if got.Written != tc.want || got.byDevice() != tc.wantDevice {
					t.Fatalf("io.stat = (%d, %q), want (%d, %q)", got.Written, got.byDevice(), tc.want, tc.wantDevice)
				}
			}
		})
	}
}

func TestParseImageProbe(t *testing.T) {
	tests := []struct {
		name   string
		out    string
		want   imageProbe
		errSub string
	}{
		{
			name: "classic store with two images",
			out:  "driver overlay2\nstore classic\nimage sha256:aa 1000\nimage sha256:bb 2500\n",
			want: imageProbe{Driver: "overlay2", Store: "classic", Images: 2, ImageBytes: 3500},
		},
		{
			name: "containerd store reports content size in KiB",
			out:  "driver overlayfs\nstore containerd\ncontent_kib 116040\nimage sha256:aa 410684018\nimage sha256:bb 13580309\n",
			want: imageProbe{Driver: "overlayfs", Store: "containerd", Images: 2, ImageBytes: 424264327, ContentBytes: 116040 * 1024, HasContent: true},
		},
		{
			name: "no images is a valid, empty daemon",
			out:  "driver overlay2\nstore classic\n",
			want: imageProbe{Driver: "overlay2", Store: "classic"},
		},
		{
			name: "a repeated image ID counts once",
			out:  "driver overlay2\nimage sha256:aa 1000\nimage sha256:aa 1000\n",
			want: imageProbe{Driver: "overlay2", Images: 1, ImageBytes: 1000},
		},
		{
			name: "blank lines and unknown records are ignored",
			out:  "\ndriver vfs\n\nfuture_field 7\nimage sha256:aa 5\n\n",
			want: imageProbe{Driver: "vfs", Images: 1, ImageBytes: 5},
		},
		{name: "empty output has no driver", out: "", errSub: "no driver record"},
		{name: "image line with a non-numeric size", out: "driver overlay2\nimage sha256:aa 12MB\n", errSub: "malformed probe line"},
		{name: "image line missing its size", out: "driver overlay2\nimage sha256:aa\n", errSub: "want 3 fields"},
		{name: "negative size", out: "driver overlay2\nimage sha256:aa -5\n", errSub: "malformed probe line"},
		{name: "driver line with extra fields", out: "driver overlay2 extra\n", errSub: "want 2 fields"},
		{name: "content size that is not a number", out: "driver overlayfs\ncontent_kib lots\n", errSub: "malformed probe line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseImageProbe(tc.out)
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("probe = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRepoFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"repo scope", []string{"PATH=/bin", "RUNNER_ORG=acme", "RUNNER_REPO=widgets"}, "acme/widgets"},
		{"org scope", []string{"RUNNER_ORG=acme"}, "acme"},
		{"enterprise scope", []string{"RUNNER_ENTERPRISE=bigco"}, "enterprises/bigco"},
		{"none", []string{"PATH=/bin", "NOEQUALS"}, ""},
		{"empty", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoFromEnv(tc.env); got != tc.want {
				t.Fatalf("repoFromEnv = %q, want %q", got, tc.want)
			}
		})
	}
}
