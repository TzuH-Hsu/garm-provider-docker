package config

import "testing"

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{name: "bare bytes", in: "1024", want: 1024},
		{name: "explicit B suffix", in: "1024B", want: 1024},
		{name: "KiB", in: "8KiB", want: 8 * 1024},
		{name: "MiB", in: "512MiB", want: 512 * 1024 * 1024},
		{name: "GiB", in: "8GiB", want: 8 * 1024 * 1024 * 1024},
		{name: "KB decimal", in: "8KB", want: 8_000},
		{name: "MB decimal", in: "512MB", want: 512_000_000},
		{name: "GB decimal", in: "4GB", want: 4_000_000_000},
		{name: "GB and GiB are distinct", in: "1GB", want: 1_000_000_000},
		{name: "lowercase unit", in: "8gib", want: 8 * 1024 * 1024 * 1024},
		{name: "mixed-case unit", in: "8GiB", want: 8 * 1024 * 1024 * 1024},
		{name: "zero bytes", in: "0", want: 0},
		{name: "whitespace around value is tolerated", in: " 8GiB ", want: 8 * 1024 * 1024 * 1024},
		{name: "whitespace between number and unit is tolerated", in: "8 GiB", want: 8 * 1024 * 1024 * 1024},
		{name: "empty string", in: "", wantErr: true},
		{name: "no digits", in: "GiB", wantErr: true},
		{name: "unrecognized unit", in: "8XiB", wantErr: true},
		{name: "fractional size rejected", in: "1.5GiB", wantErr: true},
		{name: "negative size rejected", in: "-8GiB", wantErr: true},
		{name: "overflow rejected", in: "999999999999999999999GiB", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseByteSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseByteSize(%q) = %d, nil; want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseByteSize(%q) returned unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestParsePositiveByteSize covers the H1 positive-only parse used by the
// extra_specs memory-override channel: every zero representation and any
// negative is rejected, while a genuine positive value parses as usual.
func TestParsePositiveByteSize(t *testing.T) {
	rejected := []string{"0", "00", "0GiB", "0 B", "0KiB", "-1GiB", "-8GiB", ""}
	for _, in := range rejected {
		if got, err := ParsePositiveByteSize(in); err == nil {
			t.Errorf("ParsePositiveByteSize(%q) = %d, nil; want a rejection (non-positive)", in, got)
		}
	}
	accepted := map[string]int64{
		"1":      1,
		"1B":     1,
		"512MiB": 512 * 1024 * 1024,
		"8GiB":   8 * 1024 * 1024 * 1024,
		"4GB":    4_000_000_000,
	}
	for in, want := range accepted {
		got, err := ParsePositiveByteSize(in)
		if err != nil {
			t.Errorf("ParsePositiveByteSize(%q) returned unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePositiveByteSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseByteSizeGBAndGiBAreNotAliased(t *testing.T) {
	// The load-bearing distinction this parser exists for: unlike
	// github.com/docker/go-units' RAMInBytes, "GB" must NOT parse to the
	// same value as "GiB" (see the doc comment on byteSizeUnits).
	gb, err := ParseByteSize("1GB")
	if err != nil {
		t.Fatalf("ParseByteSize(1GB) returned unexpected error: %v", err)
	}
	gib, err := ParseByteSize("1GiB")
	if err != nil {
		t.Fatalf("ParseByteSize(1GiB) returned unexpected error: %v", err)
	}
	if gb == gib {
		t.Fatalf("ParseByteSize(1GB) = %d and ParseByteSize(1GiB) = %d must differ", gb, gib)
	}
	if gb != 1_000_000_000 {
		t.Errorf("ParseByteSize(1GB) = %d, want 1_000_000_000", gb)
	}
	if gib != 1<<30 {
		t.Errorf("ParseByteSize(1GiB) = %d, want 2^30", gib)
	}
}
