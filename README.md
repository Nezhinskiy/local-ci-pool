# local-ci-pool

The Macs you already own run your private repositories' GitHub Actions jobs, in ephemeral Docker
containers, while they are awake. When no Mac is awake, GitHub-hosted runners take over. The
fallback needs no token stored in the repository.

Read [SECURITY.md](SECURITY.md) before you install anything, and the [runbook](docs/runbook.md) for
operations.

## What does not work

- No Docker inside jobs: `services:`, a job `container:`, Docker container actions and
  `docker build` do not work, because a job container gets no Docker socket.
- Linux jobs only, in the Mac's architecture: arm64 on Apple silicon.
- Private repositories of a personal account only. A public repository is refused.
- macOS only, with Docker Engine 28 or newer. Verified on Docker Desktop.
- The job runs in your marker image, not in GitHub's runner image, so the tools GitHub preinstalls
  are not there unless your image has them.
- A Mac must be awake and logged in.

Supported upstream: `actions/scaleset v0.4.0`, pinned exactly.

## Quickstart

1. Log in to GitHub and start Docker (Engine 28 or newer):

   ```sh
   gh auth login --scopes repo,workflow,read:org
   docker info
   ```

2. Install the pool. The installer downloads the latest release, verifies its build provenance, and
   starts a launchd agent. `./install.sh --dry-run` shows what it would do first.

   ```sh
   gh repo clone Nezhinskiy/local-ci-pool && cd local-ci-pool
   ./install.sh
   ```

3. Opt a repository in: commit [the marker](#the-marker), `.github/local-ci.json`, to its default
   branch. The pool finds it on its next discovery pass, within 10 minutes.

4. Route a workflow through the [`route` action](#routing): the example there has a `route` job and
   a `test` job that takes its `runs-on` from it.

5. Push, and check the pool and the run. The installed binary is not on your `PATH`:

   ```sh
   "$HOME/Library/Application Support/local-ci-pool/bin/pool" status
   ```

   ```
   pool v0.3.2 (5dbddde) on examplemacbo: docker ok, login ok, 7 slots, 0 in use, budget on
     alice/alpha  healthy
     alice/beta   healthy
   ```

   `pool status` exits 0 when the pool is healthy, 1 when it answers 503 (Docker down or `gh`
   login lost), and 3 when nothing answers. With `--json` it prints the `/healthz` body as it
   is. When `test` ran on `alpha-local` and went green, the Mac ran your job.

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

## Routing

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
          hosted-label: ubuntu-24.04-arm
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

`ubuntu-24.04-arm` runs the same architecture as an Apple silicon Mac, so a job runs on the same
instructions whether it lands on the Mac or falls back, and it costs less than the x64 label. It was
available to a private repository of a personal account on 2026-10-28. Where your account has no
arm runners, or your Mac is an Intel one, pass `hosted-label: ubuntu-24.04` instead.

Pin the action by its full commit SHA; Renovate's `github-actions` manager keeps it current.

| Input | Meaning |
|---|---|
| `vars-json` | `${{ toJSON(vars) }}`, the repository variables, which carry the heartbeats |
| `identity` | the repository identity, which is also the label prefix |
| `hosted-label` | the label to use when no Mac is fresh |
| `hosted-shards` | the shard count to use when no Mac is fresh |
| `local-label` | the label to use when a Mac is fresh; default `<identity>-local` (see [below](#use-the-routing-with-another-runner)) |
| `mode` | `auto` (default), or `hosted` to force the hosted label |

| Output | Meaning |
|---|---|
| `label` | use it directly in `runs-on`, without `fromJSON` |
| `shards` | how many parallel shards to run: the sum of the fresh machines' slots, at most 4 |
| `local` | `true` when a Mac was selected, `false` otherwise |

A heartbeat is the variable `CI_POOL_HB_<MACHINE>` with the value `"<unix epoch> <slots>"`. It is
fresh when it is at most 300 seconds old and at most 30 seconds in the future. Keys and values
that do not match that shape are ignored.

### When it falls back

When `route` falls back in `auto` mode, it says so in the run, as an annotation on the `route` job.
The jobs still run, on the hosted label. With the example above, the three cases read:

```
::notice title=local-ci-pool::no Mac heartbeat found; running on ubuntu-24.04-arm
::warning title=local-ci-pool::no fresh Mac heartbeat (the newest, CI_POOL_HB_EXAMPLEMACBO, is 1d 18h old); running on ubuntu-24.04-arm
::warning title=local-ci-pool::no fresh Mac heartbeat (the newest, CI_POOL_HB_EXAMPLEMACBO, is in the future); running on ubuntu-24.04-arm
```

- The notice: the repository has no heartbeat variable at all. No Mac has ever published one, or
  they were deleted.
- The first warning: the newest heartbeat is too old. It names the machine and its age; here the Mac
  went quiet 42 hours ago. Start with `pool status` on that Mac, and see
  [CI went hosted](docs/runbook.md#ci-went-hosted).
- The second warning: the newest heartbeat is more than 30 seconds ahead of the runner's clock.

`mode: hosted` forces the hosted label and prints no annotation.

### Use the routing with another runner

The `route` action does not care what serves the local label. For a runner that is not this pool
(deckhand, runscaler, mac-runner, a plain self-hosted runner), pass the label it registers with as
`local-label`, and publish the heartbeat with [`route/heartbeat.sh`](route/heartbeat.sh):

```yaml
        with:
          vars-json: ${{ toJSON(vars) }}
          identity: alpha
          local-label: my-mac
          hosted-label: ubuntu-24.04-arm
          hosted-shards: 4
```

```sh
route/heartbeat.sh --repo OWNER/NAME --machine MYMAC --slots 2 --check 'docker info >/dev/null'
```

It sets `CI_POOL_HB_<MACHINE>` to `"<unix epoch> <slots>"`; run it every two minutes. If `--check`
fails, nothing is written, so the routing falls back. Authenticate with a fine-grained personal
access token limited to that one repository, with *Variables: read and write*, in `GH_TOKEN`, not
with your ambient `gh` login. launchd starts a job with a minimal `PATH`, so the plist sets both. This
is the example from the script's header, which also has the cron form:

```xml
<key>ProgramArguments</key>
<array>
  <string>/path/to/heartbeat.sh</string>
  <string>--repo</string><string>OWNER/NAME</string>
  <string>--machine</string><string>MYMAC</string>
  <string>--slots</string><string>2</string>
  <string>--check</string><string>docker info >/dev/null</string>
</array>
<key>EnvironmentVariables</key>
<dict>
  <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  <key>GH_TOKEN</key><string>github_pat_...</string>
</dict>
<key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/heartbeat.err</string>
<key>StartInterval</key><integer>120</integer>
```

Keep that plist private (`chmod 600`): the token is in it. `heartbeat.sh` refuses a public repository
(exit 2), as the pool does, because a pull request from a fork would run on your machine. The check
is the repository's visibility, not its owner, so a private repository owned by an organization is
served.

## Compared with

What the neighbouring projects document, read on 2026-10-28. `unverified` means the project's
documentation did not settle it.

| Project | Host | Ephemeral | Hosted fallback | Secret needed for fallback | Personal accounts |
|---|---|---|---|---|---|
| local-ci-pool | macOS, Docker Engine 28+ | yes, a container per job | built in (`route`, heartbeat variables) | none | yes (only) |
| [deckhand](https://github.com/roark-dev/deckhand) | macOS or Linux; Colima, OrbStack, Docker Desktop | yes, a container per job (optional warm runners) | none | n/a | yes (repository scale sets; reuses the `gh` login) |
| [runscaler](https://github.com/ysya/runscaler) | Linux or macOS | yes (container or Tart macOS VM) | none | n/a | repository or organization |
| [gharp](https://github.com/muhac/actions-runner-pool) | any Docker host; needs a public HTTPS URL | yes (webhook-driven containers) | none | n/a | yes |
| [omniaura/mac-runner](https://github.com/omniaura/mac-runner) | macOS 13+ | no (persistent native runners) | via runner-fallback-action | fine-grained PAT, *Administration: Read-only* | repository or organization |
| [Tartelet](https://github.com/framna-dk/tartelet) | macOS (Tart VMs) | unverified | unverified | unverified | unverified |
| [runner-fallback-action](https://github.com/mikehardy/runner-fallback-action) | a selector, not a runner: lists the registered self-hosted runners through the REST API | n/a | yes; behaviour with a scale-to-zero pool: pending Task 10 | PAT, *Administration: Read* (repository) or organization admin | yes |
| [ARC](https://github.com/actions/actions-runner-controller) | Kubernetes | unverified | unverified | unverified | unverified |
| [rhobuild/runpool](https://github.com/rhobuild/runpool) | Linux, rootful Docker Engine 28 | yes (a capsule per job with an inner dockerd) | unverified | unverified | unverified |

## Core budget (optional)

With the core budget on, each CI job waits for its cores in the machine-wide
[coreslot](https://github.com/core-slot/coreslot) budget that your local builds, tests and agents
share, so CI and local work no longer oversubscribe the Mac. Nothing changes until you create the
settings file.

The file is `~/.config/local-ci-pool/config.toml`; when only
`~/Library/Application Support/local-ci-pool/config.toml` exists, the pool reads that one. A pool
started by hand from a shell whose `XDG_CONFIG_HOME` is an absolute path looks in
`$XDG_CONFIG_HOME/local-ci-pool/` instead of `~/.config/local-ci-pool/`; the runbook has
[the exact rule](docs/runbook.md#the-budget-settings). It has two keys:

```toml
[budget]
enabled = true
cpu = 2      # the most cores one job asks for: 1 to 256, no default
```

Start at 2, the CPUs per slot the pool's sizing assumes. A job starts once one core is free and
takes up to `cpu`; it finds its grant in `CORESLOT_CPU`, and a workflow that runs its suite with
`-n "${CORESLOT_CPU:-2}"` (or the like) stays within it. The installer writes the file for you
when there is none, and never changes one that exists:

```sh
./install.sh --budget-cpu 2
```

Edit the file to change `cpu`; the next job asks for the new value, without a restart. To turn the
budget off, set `enabled = false` or delete the file. The runbook covers
[the settings](docs/runbook.md#the-budget-settings) in full and [what to do when CI
waits](docs/runbook.md#ci-is-waiting).

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). The code that adapts `actions/scaleset` is credited in
[third_party/NOTICE](third_party/NOTICE). This project is released under the [MIT License](LICENSE).
