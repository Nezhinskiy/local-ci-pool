# Contributing

## Tests

- Run `go test -race ./...` before every change. CI runs the same command.
- CI also runs `gofmt -l .`, `go vet`, `staticcheck`, `govulncheck`, `actionlint` and `shellcheck`
  on every script. Run them locally with the same versions the workflow installs.
- A test ships with the mutation that reddens it. If you add an assertion, break the code under
  test once and watch it fail.
- Tests use a fake GitHub server and a fake Docker; none touches the network.

## Public repository hygiene

This repository is public and serves private ones. Keep every fixture, comment, document and
commit message free of private repository names, machine names and owner handles. Use neutral
names: `alpha`, `beta`, `examplemac`, `Example-MacBook`. `hygiene_test.go` rejects any `owner/repo`
reference other than this repository, and the `deny-list` CI job checks the tree, the file names
and the commit messages against a list that lives in an Actions secret.

## Workflows

- Every `runs-on` is a literal GitHub-hosted label. `hygiene_test.go` enforces it.
- Pin every action by its full commit SHA and note the version in a trailing comment.
