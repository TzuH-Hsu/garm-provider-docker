module github.com/TzuH-Hsu/garm-provider-docker

go 1.25.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/cloudbase/garm-provider-common v0.1.9
	github.com/distribution/reference v0.6.0
	// Pinned to v28.5.2+incompatible: this module path never adopted a
	// /vN import path, so the Go module proxy caps github.com/docker/docker
	// here — no v29 is reachable under this import path. This is now the
	// terminal version reachable on this path; there is no further bump
	// available here.
	//
	// v28.5.2 is also the last release carrying the back-compat shims
	// this repo depends on: types.ContainerJSON and friends are
	// deprecated type aliases whose godoc reads "Deprecated: use
	// container.InspectResponse. It will be removed in the next
	// release." Moving past this point is a real migration, not a
	// routine bump.
	//
	// Upstream split the client into separately-versioned modules:
	// github.com/moby/moby/client, github.com/moby/moby/api (which
	// holds pkg/stdcopy), and github.com/moby/moby/v2 (currently beta
	// only). Tracked in issue #1.
	//
	// Dependabot gomod VERSION updates remain intentionally disabled
	// for this reason (see .github/dependabot.yml), while gomod
	// SECURITY updates remain active.
	github.com/docker/docker v28.5.2+incompatible
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.1
	github.com/xeipuuv/gojsonschema v1.2.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/docker/go-connections v0.5.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/sys/atomicwriter v0.1.0 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/morikuni/aec v1.1.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/xeipuuv/gojsonpointer v0.0.0-20180127040702-4e3ac2762d5f // indirect
	github.com/xeipuuv/gojsonreference v0.0.0-20180127040603-bd5ef7bd5415 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)
