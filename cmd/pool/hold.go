package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// holdLine is the line hold prints once coreslot runs it.
type holdLine struct {
	Hold       string `json:"hold"`
	DockerHost string `json:"docker_host"`
	Unbudgeted string `json:"unbudgeted"`
}

// hold is the command a coreslot request runs. It reports the admission on one
// stdout line, the grant coreslot put in its environment, then waits until the
// pool closes its stdin: the room stays held while the runner runs. Nothing
// else goes to stdout.
func hold(env func(string) string, stdin io.Reader, stdout io.Writer) int {
	b, err := json.Marshal(holdLine{
		Hold:       env("CORESLOT_HOLD"),
		DockerHost: env("DOCKER_HOST"),
		Unbudgeted: env("CORESLOT_UNBUDGETED"),
	})
	if err != nil {
		return exitError
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", b); err != nil {
		return exitError
	}
	_, _ = io.Copy(io.Discard, stdin)
	return exitOK
}
