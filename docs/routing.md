# Routing

The reference for the `route` action: the sharded workflow, every input and output, the heartbeat,
and how to route to a runner that is not this pool. The [README](../README.md#routing) has the minimal
workflow and what a run says when it [falls back](../README.md#when-it-falls-back).

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
        # `shards` is 1 to 4 from a Mac, and `hosted-shards` when it falls back:
        # this map has entries up to 4, so keep `hosted-shards` at 4 or less
        # (or extend the map) or the matrix is null exactly on fallback.
        shard: ${{ fromJSON('{"1":[1],"2":[1,2],"3":[1,2,3],"4":[1,2,3,4]}')[needs.route.outputs.shards] }}
    steps:
      - run: echo "shard ${{ matrix.shard }} of ${{ needs.route.outputs.shards }}"
```

`ubuntu-24.04-arm` runs the same architecture as an Apple silicon Mac, so a job runs on the same
instructions whether it lands on the Mac or falls back, and it costs less than the x64 label. It was
available to a private repository of a personal account on 2026-10-28. Where your account has no
arm runners, or your Mac is an Intel one, pass `hosted-label: ubuntu-24.04` instead.

Pin the action by its full commit SHA; Renovate's `github-actions` manager keeps it current. Take the
SHA of the latest release (v0.4.0 or later has `local-label` and the
[fallback annotations](../README.md#when-it-falls-back)):

```sh
gh api "repos/Nezhinskiy/local-ci-pool/commits/$(gh release view --repo Nezhinskiy/local-ci-pool --json tagName --jq .tagName)" --jq .sha
```

| Input | Meaning |
|---|---|
| `vars-json` | `${{ toJSON(vars) }}`, the repository variables, which carry the heartbeats |
| `identity` | the repository identity, which is also the label prefix: 1 to 40 of `[a-z0-9-]` |
| `hosted-label` | the label to use when no Mac is fresh: 1 to 64 of `[a-z0-9.-]` |
| `hosted-shards` | the shard count to use when no Mac is fresh: 1 to 99 |
| `local-label` | the label to use when a Mac is fresh, 1 to 64 of `[a-z0-9.-]`; default `<identity>-local` (see [below](#use-the-routing-with-another-runner)) |
| `mode` | `auto` (default), or `hosted` to force the hosted label |

| Output | Meaning |
|---|---|
| `label` | use it directly in `runs-on`, without `fromJSON` |
| `shards` | how many parallel shards to run: the sum of the fresh machines' slots, at most 4, or `hosted-shards` (1 to 99) when no Mac is fresh |
| `local` | `true` when a Mac was selected, `false` otherwise |

A heartbeat is the variable `CI_POOL_HB_<MACHINE>` with the value `"<unix epoch> <slots>"`. It is
fresh when it is at most 300 seconds old and at most 30 seconds in the future. Keys and values
that do not match that shape are ignored.

## Use the routing with another runner

The `route` action does not care what serves the local label. For a runner that is not this pool
(deckhand, runscaler, mac-runner, a plain self-hosted runner), pass the label it registers with as
`local-label`, and publish the heartbeat with [`route/heartbeat.sh`](../route/heartbeat.sh):

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
(exit 2), as the pool does, because a pull request from a fork would run on your machine. Unlike the
pool, the script checks only the repository's visibility, not its owner, so it also publishes
heartbeats for a private repository owned by an organization. The pool itself serves only your
personal account's repositories.
