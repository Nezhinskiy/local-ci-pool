# The recording behind the README demo. Regenerate it with docs/demo/render.sh.
#
# Nothing here is faked or typed in by hand afterwards: every line on screen is printed by the real
# programs of this checkout, run against fixtures in a scratch directory with a scratch HOME:
#   - `pool status` is the real binary (built from ./cmd/pool). It reads a health endpoint, and that
#     endpoint is a stand-in: fake-healthz.py serves docs/demo/healthz.json (neutral names, not a
#     real pool). So the machine name, the repositories and the slot count are fixture data.
#   - `./route.sh` is the real route/route.sh, the script behind the `route` action. The heartbeat
#     variable is fixture data, and ROUTE_TEST_NOW pins the clock so the ages are exact.
#   - "The Mac goes to sleep" is the fixture server being stopped, so nothing answers.
# Not shown, on purpose: install.sh (its dry run needs the network, a release and a gh login) and a
# real pool (it would print real repository names).

Output docs/demo/demo.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 560
Set Padding 20
Set TypingSpeed 40ms
Set Framerate 25
Set Theme "Dracula"

Hide
Type "source docs/demo/setup.sh" Enter
Sleep 8s
Type "clear" Enter
Sleep 500ms
Show

Type "# A Mac with the pool running serves two private repositories." Enter
Sleep 600ms
Type "pool status --health-addr 127.0.0.1:18477" Enter
Sleep 4s

Type "# Its heartbeat is a repository variable: unix time and slots. This one is 60 s old." Enter
Sleep 600ms
Type `VARS_JSON='{"CI_POOL_HB_EXAMPLEMAC":"1792999940 7"}' ./route.sh` Enter
Sleep 5s

Hide
Type "sleep_mac; clear" Enter
Sleep 500ms
Show
Type "# The Mac goes to sleep. Nothing answers any more." Enter
Sleep 600ms
Type "pool status --health-addr 127.0.0.1:18477; echo exit $?" Enter
Sleep 4s

Type "# Ten minutes on, the last heartbeat is stale." Enter
Sleep 600ms
Type `VARS_JSON='{"CI_POOL_HB_EXAMPLEMAC":"1792999400 7"}' ./route.sh` Enter
Sleep 7s

Hide
Type "demo_cleanup" Enter
Sleep 1s
