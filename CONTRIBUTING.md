# Contributing

Thanks for helping. This is a small project with one maintainer, so a short
conversation before a large change saves everyone time.

- **Small fixes:** open a pull request.
- **New features or behaviour changes:** open an issue first.
- **Questions:** use [Discussions](https://github.com/TzuH-Hsu/garm-provider-docker/discussions).
- **Security problems:** follow [SECURITY.md](SECURITY.md). Do not open a public issue.

Scheduling policy, such as limits across a whole host or fleet, belongs in
GARM rather than in this provider.

## Set up

You need the Go version named in [`go.mod`](go.mod). Docker is needed only for
the live test suite.

```sh
git clone https://github.com/TzuH-Hsu/garm-provider-docker
cd garm-provider-docker
go build -o garm-provider-docker .
```

## Test

These are the checks CI runs on every pull request:

```sh
gofmt -l .
go vet ./...
go test ./... -race -count=1
```

The live suite creates real containers, networks and volumes, so it needs a
Docker daemon. Run it on a machine you can spare, not on a host that serves
production jobs:

```sh
go test -tags dockerverify -v -timeout 25m ./internal/verify/...
```

[`docs/demo-m0.md`](docs/demo-m0.md) describes the end-to-end run against a
real GARM and GitHub. If your change affects the runner lifecycle, say whether
you ran it.

## Project layout

| Path | What it holds |
|---|---|
| `main.go` | Entry point; dispatches GARM commands |
| `internal/garmenv` | Reads GARM's execution environment |
| `internal/config` | Provider config and its JSON Schema |
| `internal/extraspecs` | Per-pool `extra_specs` validation |
| `internal/provider` | Implements GARM's commands: create, delete, list, status |
| `internal/spec` | Builds container, network and volume specs |
| `internal/topology` | Creates and tears down each job's Docker resources |
| `internal/docker` | Narrow Docker client interface, with a fake for tests |
| `internal/metadata` | Calls GARM's metadata and status endpoints |
| `internal/logging` | Logging and safe handling of runner log lines |
| `internal/verify` | Live tests, behind the `dockerverify` build tag |
| `runner-images/` | Runner images and their entrypoint |
| `docs/adr/` | Design decisions and their reasoning |

## Branches and commits

Name branches `<type>/<short-description>`, for example
`fix/runner-localhost-ipv4`. Write commits in the same form:

```text
<type>: <what changed, in the imperative>
```

| Type | Use for |
|---|---|
| `feat` | New behaviour |
| `fix` | Bug fixes |
| `docs` | Documentation only |
| `refactor` | Code changes with no behaviour change |
| `test` | Tests only |
| `perf` | Performance |
| `ci` | Workflows and repository automation |
| `chore` | Maintenance and dependencies |

Please don't use tool-generated branch names such as `claude/...`, `codex/...`
or `copilot/...`.

## Pull requests

- Keep each pull request to one change.
- Explain the problem and how you tested the fix.
- Add tests for behaviour changes. Update the docs when behaviour or
  configuration changes.
- CI must pass. Pull requests are merged by rebase, so keep the commits clean.

## AI tools

AI tools are welcome. If one wrote a meaningful part of your change, say so in
the PR. Test it, understand it, and be ready to explain it in review. Please
check anything your own agent opens or posts before it goes out.

## License

By contributing, you agree that your contribution is licensed under
[Apache-2.0](LICENSE).
