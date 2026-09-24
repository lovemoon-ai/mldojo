# Contributing to MLDojo

Thanks for your interest in MLDojo! Bug reports, fixes, docs and new queue plugins are all welcome.

## Getting started

Requirements:
- Go (the version in `go.mod`)
- Node.js 20+ and npm (for the web UI)
- Python 3.10+ (for the Python SDK and the mock queue plugin)
- Docker, optional (`mldojo dev up` runs PostgreSQL in docker)

```bash
git clone https://github.com/lovemoon-ai/mldojo.git
cd mldojo
make build                     # native binaries in bin/
bin/mldojo dev up              # postgres (docker) + api + web dev server, in the foreground
```

See [docs/quickstart.md](docs/quickstart.md) for a full walkthrough and [docs/architecture.md](docs/architecture.md)
for how the pieces fit together.

## Build and test

Before opening a pull request, make sure these pass:

```bash
make build                                      # Go binaries
go test ./...                                   # Go unit tests (make test)
gofmt -l agent api cli internal proto recipes adapters sdk   # must print nothing; `make fmt` fixes it
go vet ./...                                    # make vet
cd web && npm ci && npx tsc --noEmit && npm run build        # web type check + build
```

Optional:
- `make test-sidecar`: tests for the mock queue plugin.
- Integration tests run when `MLDOJO_TEST_DATABASE_URL` points to a PostgreSQL database.
- `scripts/e2e.sh`: end-to-end test that starts an isolated instance on the current host and cleans up afterwards.

## Pull requests

- Fork the repo and create a topic branch. Keep each PR focused on one change; small PRs get reviewed faster.
- Use [Conventional Commits](https://www.conventionalcommits.org/)-style messages, as in the existing history:
  `feat(cli): ...`, `fix(api): ...`, `docs: ...`, `test(e2e): ...`.
- Add or update tests for behavior changes.
- Update the docs when you change user-facing behavior. Docs are bilingual: English in `X.md`, Simplified Chinese
  in `X.zh-CN.md`. Update both if you can; if you can only write one, say so in the PR and a maintainer will help.
- Don't commit site-specific data (internal hostnames, IPs, tokens, usernames). Site settings belong in the
  gitignored `deploy/site.env`.
- CI must be green before a PR is merged. PRs are usually squash-merged.

## Reporting bugs and requesting features

Open a GitHub issue with steps to reproduce, what you expected and what happened, and the output of
`mldojo --version` / `mldojo health` where relevant. **Do not report security vulnerabilities in public issues**;
see [SECURITY.md](SECURITY.md).

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). By participating you agree to uphold it.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
