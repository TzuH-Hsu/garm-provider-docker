package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xeipuuv/gojsonschema"
)

const loggingTestRunnerImage = `runner_image = "ghcr.io/example/runner@sha256:0000000000000000000000000000000000000000000000000000000000000000"` + "\n"

func loadLoggingTOML(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(loggingTestRunnerImage+body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoggingFileUnsetByDefault(t *testing.T) {
	if got := loadLoggingTOML(t, "").Logging.File; got != "" {
		t.Errorf("Logging.File = %q, want empty when [logging] is omitted", got)
	}
	if got := loadLoggingTOML(t, "[logging]\n").Logging.File; got != "" {
		t.Errorf("Logging.File = %q, want empty when file is omitted", got)
	}
}

func TestLoggingFileIsRead(t *testing.T) {
	const want = "/var/log/garm-provider-docker/provider.log"
	got := loadLoggingTOML(t, "[logging]\nfile = \""+want+"\"\n").Logging.File
	if got != want {
		t.Errorf("Logging.File = %q, want %q", got, want)
	}
}

func TestLoggingSchemaAcceptsFileAndRejectsUnknownKey(t *testing.T) {
	schema, err := gojsonschema.NewSchema(gojsonschema.NewStringLoader(JSONSchema()))
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	cases := map[string]bool{
		`{"runner_image":"x","logging":{"file":"/var/log/p.log"}}`: true,
		`{"runner_image":"x","logging":{}}`:                        true,
		`{"runner_image":"x","logging":{"file":5}}`:                false,
		`{"runner_image":"x","logging":{"fle":"/var/log/p.log"}}`:  false,
	}
	for doc, wantValid := range cases {
		res, err := schema.Validate(gojsonschema.NewStringLoader(doc))
		if err != nil {
			t.Fatalf("validate %s: %v", doc, err)
		}
		if res.Valid() != wantValid {
			t.Errorf("schema validity of %s = %v, want %v (%v)", doc, res.Valid(), wantValid, res.Errors())
		}
	}
}
