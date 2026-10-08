# local-ci-pool

Run the GitHub Actions jobs of your private repositories on ephemeral Docker runners on your own
Macs, and fall back to GitHub-hosted runners when no Mac is available.

## Scope

- macOS with Docker Desktop. Linux job containers only.
- A personal account: repository-level scale sets for the private repositories that account owns.
- Private repositories only. A public repository is never served.
- Supported upstream: `actions/scaleset v0.4.0`, pinned exactly.

Read [SECURITY.md](SECURITY.md) before you install anything, and the [runbook](docs/runbook.md) for
operations.

## How it works

1. A repository opts in by committing a marker, `.github/local-ci.json`, on its default branch.
2. A `pool` program on each Mac registers one scale set per served repository and starts one
   ephemeral runner container per job, up to the machine's slot limit.
3. Each Mac publishes a heartbeat as a repository variable. A hosted `route` job reads the
   heartbeats and decides whether the expensive jobs run on a Mac or on a hosted runner.

### The marker

The marker takes one of two forms. With a prebuilt image, pinned by digest:

```json
{ "image": "ghcr.io/example/ci-image:1.0@sha256:0000000000000000000000000000000000000000000000000000000000000000" }
```

Or built from the repository itself, where `inputs` lists the repository paths that make up the
build context (and so define the image tag):

```json
{ "dockerfile": "ci/Dockerfile", "inputs": ["ci", "requirements.txt"] }
```

The repository's identity is its name reduced to `[a-z0-9-]`, for example `alpha`.

### Slots

A Mac runs at most `slots` jobs at once:

```
slots = min(floor((round(MemTotal / 2^30) - 2) / 4), floor(NCPU / 2), 8)
```

`MemTotal` and `NCPU` come from `docker info`. The memory term rounds to the nearest GiB because the
Docker Desktop VM reports slightly less than its slider value. The pool refuses to start when
`slots` is below 1.

### Routing

All machines of a repository share one label, `<identity>-local` (for the identity `alpha`:
`alpha-local`). The `route` action emits that label when at least one Mac has a fresh heartbeat,
and the hosted label you pass otherwise.

```yaml
jobs:
  route:
    runs-on: ubuntu-24.04
    outputs:
      label: ${{ steps.route.outputs.label }}
      shards: ${{ steps.route.outputs.shards }}
      local: ${{ steps.route.outputs.local }}
    steps:
      - id: route
        uses: Nezhinskiy/local-ci-pool/route@<40-hex sha>
        with:
          vars-json: ${{ toJSON(vars) }}
          identity: alpha
          hosted-label: ubuntu-24.04
          hosted-shards: 4

  test:
    needs: route
    runs-on: ${{ needs.route.outputs.label }}
    strategy:
      matrix:
        shard: [1, 2, 3, 4]
    steps:
      - run: echo "shard ${{ matrix.shard }} of ${{ needs.route.outputs.shards }}"
```

Pin the action by its full commit SHA; Renovate's `github-actions` manager keeps it current.

| Input | Meaning |
|---|---|
| `vars-json` | `${{ toJSON(vars) }}`, the repository variables, which carry the heartbeats |
| `identity` | the repository identity, which is also the label prefix |
| `hosted-label` | the label to use when no Mac is fresh |
| `hosted-shards` | the shard count to use when no Mac is fresh |
| `mode` | `auto` (default), or `hosted` to force the hosted label |

| Output | Meaning |
|---|---|
| `label` | use it directly in `runs-on`, without `fromJSON` |
| `shards` | how many parallel shards to run: the sum of the fresh machines' slots, at most 4 |
| `local` | `true` when a Mac was selected, `false` otherwise |

A heartbeat is the variable `CI_POOL_HB_<MACHINE>` with the value `"<unix epoch> <slots>"`. It is
fresh when it is at most 300 seconds old and at most 30 seconds in the future. Keys and values
that do not match that shape are ignored.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). The code that adapts `actions/scaleset` is credited in
[third_party/NOTICE](third_party/NOTICE). This project is released under the [MIT License](LICENSE).
