# Contributing

## Tests

- Run `go test -race ./...` before every change. CI runs the same command.
- CI also runs `gofmt -l .`, `go vet`, `staticcheck`, `govulncheck`, `actionlint` and `shellcheck`
  on every script. Run them locally with the same versions the workflow installs.
- A test ships with the mutation that reddens it. If you add an assertion, break the code under
  test once and watch it fail.
- Tests use a fake GitHub server and a fake Docker; none touches the network.
- `go test -tags docker ./...` adds the smoke tests against the real Docker daemon. They need Docker
  running and network access: they download the actions/runner release and build the runner image.
  CI does not run them; run them before a change to the runner mount, the entrypoint or the container
  spec.

## Public repository hygiene

This repository is public and serves private ones. Keep every fixture, comment, document and
commit message free of private repository names, machine names and owner handles. Use neutral
names: `alpha`, `beta`, `examplemac`, `Example-MacBook`. `hygiene_test.go` rejects any `owner/repo`
reference other than this repository, and the `deny-list` CI job (`scripts/deny-list.sh`) checks the
tree, the file names, each pushed commit's message, author and committer, and every annotated tag's
name, tagger and message against a list that lives in an Actions secret.

## Workflows

- Every `runs-on` is a literal GitHub-hosted label. `hygiene_test.go` enforces it.
- Pin every action by its full commit SHA and note the version in a trailing comment.
