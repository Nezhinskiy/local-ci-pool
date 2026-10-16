package budget

// State is the budget's part of /healthz. It carries the pool's own data only: never a path, a
// hold's label or the library's reason string.
type State struct {
	State       string `json:"state"`           // off, on, wedged
	Error       string `json:"error,omitempty"` // config, open, state
	OldestWaitS int    `json:"oldest_wait_s"`
}
