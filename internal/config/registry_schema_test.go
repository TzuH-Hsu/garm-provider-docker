package config

import (
	"testing"

	"github.com/xeipuuv/gojsonschema"
)

// registryParityCase is one list value run through BOTH the published config
// schema and the loader's validation.
type registryParityCase struct {
	name    string
	entries []string
	// schemaOK/loaderOK are the expected verdicts of each side.
	schemaOK bool
	loaderOK bool
	// loaderOnly marks a rule JSON Schema cannot express (host or port
	// semantics, duplicates that differ by case or form): the schema is
	// intentionally looser there, so schemaOK may be true while loaderOK is
	// false. Every other row must have both sides agree.
	loaderOnly bool
}

// TestRegistrySchemaLoaderParity pins that the published schema and the loader
// agree on registry_mirrors and insecure_registries for every rule JSON Schema
// can express, and documents (loaderOnly) the ones it cannot. It exists because
// the schema is hand-maintained and once accepted an uppercase scheme the
// loader rejects.
func TestRegistrySchemaLoaderParity(t *testing.T) {
	schema, err := gojsonschema.NewSchema(gojsonschema.NewStringLoader(JSONSchema()))
	if err != nil {
		t.Fatalf("published config schema does not compile: %v", err)
	}

	mirrorCases := []registryParityCase{
		{name: "https host", entries: []string{"https://mirror.gcr.io"}, schemaOK: true, loaderOK: true},
		{name: "http host and port", entries: []string{"http://mirror.lan.example:5000"}, schemaOK: true, loaderOK: true},
		{name: "trailing slash", entries: []string{"https://mirror.gcr.io/"}, schemaOK: true, loaderOK: true},
		{name: "IPv4 and port", entries: []string{"http://10.0.0.5:5000"}, schemaOK: true, loaderOK: true},
		{name: "bracketed IPv6 and port", entries: []string{"http://[fd00::5]:5000"}, schemaOK: true, loaderOK: true},
		{name: "empty list", entries: []string{}, schemaOK: true, loaderOK: true},
		{name: "two distinct", entries: []string{"https://a.example", "https://b.example"}, schemaOK: true, loaderOK: true},

		{name: "uppercase scheme", entries: []string{"HTTPS://mirror.example"}},
		{name: "mixed-case scheme", entries: []string{"Http://mirror.example"}},
		{name: "no scheme", entries: []string{"mirror.lan.example:5000"}},
		{name: "ftp scheme", entries: []string{"ftp://mirror.example"}},
		{name: "empty entry", entries: []string{""}},
		{name: "leading space", entries: []string{" https://mirror.example"}},
		{name: "trailing space", entries: []string{"https://mirror.example "}},
		{name: "path", entries: []string{"https://mirror.example/dockerhub"}},
		{name: "query", entries: []string{"https://mirror.example/?x=1"}},
		{name: "empty query marker", entries: []string{"https://mirror.example?"}},
		{name: "fragment", entries: []string{"https://mirror.example#f"}},
		{name: "userinfo", entries: []string{"https://user:pass@mirror.example"}},
		{name: "percent-encoding", entries: []string{"https://mirror%2Eexample"}},
		{name: "exact duplicate", entries: []string{"https://a.example", "https://a.example"}},

		{name: "empty host", entries: []string{"http://:5000"}, schemaOK: true, loaderOnly: true},
		{name: "port out of range", entries: []string{"http://mirror.example:70000"}, schemaOK: true, loaderOnly: true},
		{name: "port zero", entries: []string{"http://mirror.example:0"}, schemaOK: true, loaderOnly: true},
		{name: "non-numeric port", entries: []string{"http://mirror.example:abc"}, schemaOK: true, loaderOnly: true},
		{name: "invalid host characters", entries: []string{"http://mirror_host!.example"}, schemaOK: true, loaderOnly: true},
		{name: "duplicate by host case", entries: []string{"https://a.example", "https://A.example"}, schemaOK: true, loaderOnly: true},
		{name: "duplicate by trailing slash", entries: []string{"https://a.example", "https://a.example/"}, schemaOK: true, loaderOnly: true},
	}

	insecureCases := []registryParityCase{
		{name: "host", entries: []string{"mirror.lan.example"}, schemaOK: true, loaderOK: true},
		{name: "host and port", entries: []string{"mirror.lan.example:5000"}, schemaOK: true, loaderOK: true},
		{name: "IPv4 and port", entries: []string{"10.0.0.5:5000"}, schemaOK: true, loaderOK: true},
		{name: "bracketed IPv6 and port", entries: []string{"[fd00::5]:5000"}, schemaOK: true, loaderOK: true},
		{name: "bare IPv6", entries: []string{"fd00::5"}, schemaOK: true, loaderOK: true},
		{name: "IPv4 CIDR", entries: []string{"192.168.0.0/16"}, schemaOK: true, loaderOK: true},
		{name: "IPv6 CIDR", entries: []string{"fd00::/8"}, schemaOK: true, loaderOK: true},
		{name: "empty list", entries: []string{}, schemaOK: true, loaderOK: true},
		{name: "two distinct", entries: []string{"a.example:5000", "10.0.0.0/8"}, schemaOK: true, loaderOK: true},

		{name: "empty entry", entries: []string{""}},
		{name: "blank entry", entries: []string{" "}},
		{name: "space inside", entries: []string{"mirror example"}},
		{name: "http scheme", entries: []string{"http://mirror.example"}},
		{name: "https scheme", entries: []string{"https://mirror.example:5000"}},
		{name: "path", entries: []string{"mirror.example:5000/v2"}},
		{name: "leading hyphen", entries: []string{"-mirror.example"}},
		{name: "trailing hyphen", entries: []string{"mirror.example-"}},
		{name: "userinfo", entries: []string{"user:pass@mirror.example:5000"}},
		{name: "percent-encoding", entries: []string{"mirror%40example"}},
		{name: "exact duplicate", entries: []string{"a.example:5000", "a.example:5000"}},

		{name: "port out of range", entries: []string{"mirror.example:70000"}, schemaOK: true, loaderOnly: true},
		{name: "non-numeric port", entries: []string{"mirror.example:abc"}, schemaOK: true, loaderOnly: true},
		{name: "invalid host characters", entries: []string{"mirror_host!.example"}, schemaOK: true, loaderOnly: true},
		{name: "CIDR prefix too long", entries: []string{"10.0.0.0/33"}, schemaOK: true, loaderOnly: true},
		{name: "duplicate by case", entries: []string{"A.example:5000", "a.example:5000"}, schemaOK: true, loaderOnly: true},
	}

	run := func(t *testing.T, key string, cases []registryParityCase, set func(*Config, []string)) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if !tc.loaderOnly && tc.schemaOK != tc.loaderOK {
					t.Fatalf("bad table row: schemaOK=%v loaderOK=%v disagree without loaderOnly", tc.schemaOK, tc.loaderOK)
				}

				doc := map[string]any{"runner_image": registryTestRunnerImage, key: tc.entries}
				res, err := schema.Validate(gojsonschema.NewGoLoader(doc))
				if err != nil {
					t.Fatalf("schema validation errored: %v", err)
				}
				if res.Valid() != tc.schemaOK {
					t.Errorf("schema valid = %v, want %v (errors: %v)", res.Valid(), tc.schemaOK, res.Errors())
				}

				cfg := validBaseConfig(registryTestRunnerImage, false)
				set(&cfg, tc.entries)
				if got := cfg.Validate() == nil; got != tc.loaderOK {
					t.Errorf("loader valid = %v, want %v", got, tc.loaderOK)
				}
			})
		}
	}

	t.Run("registry_mirrors", func(t *testing.T) {
		run(t, "registry_mirrors", mirrorCases, func(c *Config, e []string) { c.RegistryMirrors = e })
	})
	t.Run("insecure_registries", func(t *testing.T) {
		run(t, "insecure_registries", insecureCases, func(c *Config, e []string) { c.InsecureRegistries = e })
	})
}
