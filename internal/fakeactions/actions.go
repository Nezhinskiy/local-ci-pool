package fakeactions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// The responses below follow what github.com answered in the Wave A probes:
// the session conflict is a 409 carrying RunnerScaleSetSessionConflictException,
// the same for a stale session of the same owner and for another live owner.
const (
	conflictException     = "RunnerScaleSetSessionConflictException"
	stillRunningException = "JobStillRunningException"
	// DefaultPoll is how long a message request waits for a message before it
	// answers 202 (no message), the listener's nil-message return. The real
	// service waits about 50 s.
	DefaultPoll = 20 * time.Millisecond
)

// Actions is a fake Actions service holding runner scale sets, their
// sessions and message queues, and the runners registered to them. Tests
// script it with the methods below; every request is logged.
type Actions struct {
	*actionsServer
	t testing.TB

	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every queued message

	poll         time.Duration
	scaleSets    map[int]*scaleset.RunnerScaleSet
	nextSet      int
	sessions     map[int]uuid.UUID // scale set -> the open session
	assigned     map[int]int       // scale set -> TotalAssignedJobs
	queues       map[int][]*message
	nextMessage  int
	nextRequest  int64
	runners      map[string]*registered
	nextRunner   int
	conflicts    int // the next n session creates answer 409; < 0 always
	sessionGate  chan struct{}
	failGets     int
	stillRunning int // the next n scale set deletes answer JobStillRunning; < 0 always
	staleOnOpen  bool
	rejected     map[string]bool // tokens the registration endpoint refuses
	rejectAll    bool
	tokens       []string
	log          []string
}

type registered struct {
	id       int
	scaleSet int
	busy     bool
}

type message struct {
	id    int
	stats scaleset.RunnerScaleSetStatistic
	body  []any
}

// New starts a fake Actions service; it is closed when the test ends.
func New(t testing.TB) *Actions {
	a := &Actions{
		t:         t,
		changed:   make(chan struct{}),
		poll:      DefaultPoll,
		scaleSets: map[int]*scaleset.RunnerScaleSet{},
		nextSet:   1,
		sessions:  map[int]uuid.UUID{},
		assigned:  map[int]int{},
		queues:    map[int][]*message{},
		runners:   map[string]*registered{},
		rejected:  map[string]bool{},
	}
	a.actionsServer = newServer(t, http.HandlerFunc(a.serve), WithRunnerRegistrationTokenHandler(a.registrationToken))
	return a
}

// ConfigURL is the GitHub configuration URL of a repository (owner/name).
func (a *Actions) ConfigURL(repo string) string { return a.URL + "/" + repo }

// ---- scripting ----

// AddScaleSet creates a scale set as if a previous run had, and returns its ID.
func (a *Actions) AddScaleSet(name string, labels ...string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	ss := &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: 1}
	for _, l := range labels {
		ss.Labels = append(ss.Labels, scaleset.Label{Type: "System", Name: l})
	}
	return a.addLocked(ss)
}

func (a *Actions) addLocked(ss *scaleset.RunnerScaleSet) int {
	ss.ID = a.nextSet
	a.nextSet++
	a.scaleSets[ss.ID] = ss
	return ss.ID
}

// ScaleSet returns a copy of the scale set with the name, or nil.
func (a *Actions) ScaleSet(name string) *scaleset.RunnerScaleSet {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id := a.idLocked(name); id != 0 {
		cp := *a.scaleSets[id]
		cp.Labels = append([]scaleset.Label(nil), cp.Labels...)
		return &cp
	}
	return nil
}

func (a *Actions) idLocked(name string) int {
	for id, ss := range a.scaleSets {
		if ss.Name == name {
			return id
		}
	}
	return 0
}

// SessionOpen reports whether the named scale set has an open session.
func (a *Actions) SessionOpen(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[a.idLocked(name)]
	return ok
}

// SessionConflicts makes the next n session creates answer the measured 409;
// a negative n makes every create answer it.
func (a *Actions) SessionConflicts(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conflicts = n
}

// GateSessions holds every session create until the returned function is
// called.
func (a *Actions) GateSessions() (release func()) {
	gate := make(chan struct{})
	a.mu.Lock()
	a.sessionGate = gate
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.sessionGate = nil
			a.mu.Unlock()
			close(gate)
		})
	}
}

// StaleOnOpen queues, on the next session create, a message with a JobStarted
// for a runner with no name and a JobCompleted for a runner of a previous
// process, as github.com delivers after a restart.
func (a *Actions) StaleOnOpen() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.staleOnOpen = true
}

// FailNextGets makes the next n message requests answer 400.
func (a *Actions) FailNextGets(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failGets = n
}

// DeleteStillRunning makes the next n scale set deletes answer
// JobStillRunning; a negative n makes every delete answer it.
func (a *Actions) DeleteStillRunning(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stillRunning = n
}

// RejectTokens makes the registration endpoint answer 401 for these personal
// access tokens; with no tokens, it refuses every token.
func (a *Actions) RejectTokens(tokens ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(tokens) == 0 {
		a.rejectAll = true
	}
	for _, t := range tokens {
		a.rejected[t] = true
	}
}

// Tokens lists the personal access tokens the registration endpoint saw.
func (a *Actions) Tokens() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.tokens...)
}

// Assign changes the scale set's assigned jobs by delta and queues a message
// with the new statistics; a positive delta also carries a JobAvailable and a
// JobAssigned per new job.
func (a *Actions) Assign(name string, delta int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.mustIDLocked(name)
	a.assigned[id] = max(a.assigned[id]+delta, 0)
	var body []any
	for range max(delta, 0) {
		a.nextRequest++
		base := scaleset.JobMessageBase{RunnerRequestID: a.nextRequest, JobID: fmt.Sprintf("job-%d", a.nextRequest)}
		avail := scaleset.JobAvailable{JobMessageBase: base}
		avail.MessageType = scaleset.MessageTypeJobAvailable
		assigned := scaleset.JobAssigned{JobMessageBase: base}
		assigned.MessageType = scaleset.MessageTypeJobAssigned
		body = append(body, avail, assigned)
	}
	a.queueLocked(id, body)
}

// Started marks the runner busy and queues a JobStarted for it.
func (a *Actions) Started(name, runner string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.mustIDLocked(name)
	if r := a.runners[runner]; r != nil {
		r.busy = true
	}
	m := scaleset.JobStarted{RunnerName: runner}
	m.MessageType = scaleset.MessageTypeJobStarted
	a.queueLocked(id, []any{m})
}

// Completed clears the runner's busy mark, takes one job off the scale set's
// assigned count and queues a JobCompleted for it.
func (a *Actions) Completed(name, runner string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.mustIDLocked(name)
	if r := a.runners[runner]; r != nil {
		r.busy = false
	}
	a.assigned[id] = max(a.assigned[id]-1, 0)
	m := scaleset.JobCompleted{RunnerName: runner, Result: "succeeded"}
	m.MessageType = scaleset.MessageTypeJobCompleted
	a.queueLocked(id, []any{m})
}

// ClearBusy marks every runner idle and ends any scripted JobStillRunning, as
// if every job ended.
func (a *Actions) ClearBusy() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.runners {
		r.busy = false
	}
	a.stillRunning = 0
}

// Pending is the number of messages queued for the scale set and not yet
// delivered.
func (a *Actions) Pending(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queues[a.idLocked(name)])
}

// Runners lists the runner names registered to the scale set.
func (a *Actions) Runners(name string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.idLocked(name)
	var out []string
	for n, r := range a.runners {
		if r.scaleSet == id {
			out = append(out, n)
		}
	}
	return out
}

// Log returns the requests served, one line each, such as
// "DELETE session alpha-examplemac".
func (a *Actions) Log() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.log...)
}

// Count is the number of logged requests equal to line.
func (a *Actions) Count(line string) int {
	n := 0
	for _, l := range a.Log() {
		if l == line {
			n++
		}
	}
	return n
}

func (a *Actions) mustIDLocked(name string) int {
	id := a.idLocked(name)
	if id == 0 {
		a.t.Errorf("fakeactions: no scale set %q", name)
	}
	return id
}

func (a *Actions) queueLocked(id int, body []any) {
	a.nextMessage++
	a.queues[id] = append(a.queues[id], &message{id: a.nextMessage, stats: a.statsLocked(id), body: body})
	close(a.changed)
	a.changed = make(chan struct{})
}

func (a *Actions) statsLocked(id int) scaleset.RunnerScaleSetStatistic {
	return scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: a.assigned[id]}
}

func (a *Actions) logLocked(format string, args ...any) {
	a.log = append(a.log, fmt.Sprintf(format, args...))
}

// ---- the service ----

func (a *Actions) registrationToken(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	a.mu.Lock()
	a.tokens = append(a.tokens, tok)
	reject := a.rejectAll || a.rejected[tok]
	a.mu.Unlock()
	if reject {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"token": "registration-token"})
}

var (
	scaleSetsPath = regexp.MustCompile(`/_apis/runtime/runnerscalesets(?:/(\d+)(?:/(sessions|acquirejobs|generatejitconfig)(?:/([0-9a-f-]+))?)?)?$`)
	agentsPath    = regexp.MustCompile(`/_apis/distributedtask/pools/0/agents(?:/(\d+))?$`)
	queuePath     = regexp.MustCompile(`^/queue/(\d+)(?:/(\d+))?$`)
)

func (a *Actions) serve(w http.ResponseWriter, r *http.Request) {
	if m := queuePath.FindStringSubmatch(r.URL.Path); m != nil {
		id, _ := strconv.Atoi(m[1])
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		a.getMessage(w, r, id)
		return
	}
	if m := agentsPath.FindStringSubmatch(r.URL.Path); m != nil {
		a.agents(w, r, m[1])
		return
	}
	m := scaleSetsPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		a.t.Errorf("fakeactions: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id, _ := strconv.Atoi(m[1])
	switch {
	case m[1] == "" && r.Method == http.MethodGet:
		a.getScaleSet(w, r.URL.Query().Get("name"))
	case m[1] == "" && r.Method == http.MethodPost:
		a.createScaleSet(w, r)
	case m[2] == "" && r.Method == http.MethodDelete:
		a.deleteScaleSet(w, id)
	case m[2] == "sessions" && r.Method == http.MethodPost:
		a.createSession(w, r, id)
	case m[2] == "sessions" && r.Method == http.MethodDelete:
		a.deleteSession(w, id, m[3])
	case m[2] == "acquirejobs":
		var ids []int64
		_ = json.NewDecoder(r.Body).Decode(&ids)
		writeJSON(w, http.StatusOK, map[string]any{"count": len(ids), "value": ids})
	case m[2] == "generatejitconfig":
		a.generateJIT(w, r, id)
	default:
		a.t.Errorf("fakeactions: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (a *Actions) getScaleSet(w http.ResponseWriter, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logLocked("GET scaleset %s", name)
	var found []scaleset.RunnerScaleSet
	if id := a.idLocked(name); id != 0 {
		found = append(found, *a.scaleSets[id])
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(found), "value": found})
}

func (a *Actions) createScaleSet(w http.ResponseWriter, r *http.Request) {
	var ss scaleset.RunnerScaleSet
	if err := json.NewDecoder(r.Body).Decode(&ss); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logLocked("POST scaleset %s", ss.Name)
	if a.idLocked(ss.Name) != 0 {
		writeException(w, http.StatusConflict, "RunnerScaleSetExistsException", "exists")
		return
	}
	a.addLocked(&ss)
	writeJSON(w, http.StatusOK, ss)
}

func (a *Actions) deleteScaleSet(w http.ResponseWriter, id int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ss := a.scaleSets[id]
	if ss == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
		return
	}
	a.logLocked("DELETE scaleset %s", ss.Name)
	busy := false
	for _, r := range a.runners {
		busy = busy || (r.scaleSet == id && r.busy)
	}
	if a.stillRunning != 0 || busy {
		if a.stillRunning > 0 {
			a.stillRunning--
		}
		writeException(w, http.StatusBadRequest, stillRunningException, "a job is still running")
		return
	}
	delete(a.scaleSets, id)
	delete(a.sessions, id)
	for n, r := range a.runners {
		if r.scaleSet == id {
			delete(a.runners, n)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Actions) createSession(w http.ResponseWriter, r *http.Request, id int) {
	a.mu.Lock()
	gate := a.sessionGate
	a.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	var req scaleset.RunnerScaleSetSession
	_ = json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	defer a.mu.Unlock()
	ss := a.scaleSets[id]
	if ss == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
		return
	}
	a.logLocked("POST session %s", ss.Name)
	_, open := a.sessions[id]
	if a.conflicts != 0 || open {
		if a.conflicts > 0 {
			a.conflicts--
		}
		writeException(w, http.StatusConflict, conflictException,
			"The actions runner scaleset "+ss.Name+" already has an active session.")
		return
	}
	sid := uuid.New()
	a.sessions[id] = sid
	a.queues[id] = nil
	if a.staleOnOpen {
		a.staleOnOpen = false
		started := scaleset.JobStarted{RunnerName: ""}
		started.MessageType = scaleset.MessageTypeJobStarted
		done := scaleset.JobCompleted{RunnerName: "local-ci-0123456789ab", Result: "succeeded"}
		done.MessageType = scaleset.MessageTypeJobCompleted
		a.queueLocked(id, []any{started, done})
	}
	stats := a.statsLocked(id)
	writeJSON(w, http.StatusOK, scaleset.RunnerScaleSetSession{
		SessionID:               sid,
		OwnerName:               req.OwnerName,
		RunnerScaleSet:          ss,
		MessageQueueURL:         a.URL + "/queue/" + strconv.Itoa(id),
		MessageQueueAccessToken: "queue-token",
		Statistics:              &stats,
	})
}

func (a *Actions) deleteSession(w http.ResponseWriter, id int, sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ss := a.scaleSets[id]
	if ss == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
		return
	}
	a.logLocked("DELETE session %s", ss.Name)
	if cur, ok := a.sessions[id]; ok && cur.String() == sid {
		delete(a.sessions, id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Actions) getMessage(w http.ResponseWriter, r *http.Request, id int) {
	deadline := time.After(a.poll)
	for {
		a.mu.Lock()
		if a.failGets > 0 {
			a.failGets--
			a.mu.Unlock()
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "scripted failure"})
			return
		}
		_, open := a.sessions[id]
		capacity := r.Header.Get(scaleset.HeaderScaleSetMaxCapacity)
		if open && capacity != "0" && len(a.queues[id]) > 0 {
			m := a.queues[id][0]
			a.queues[id] = a.queues[id][1:]
			a.mu.Unlock()
			a.writeMessage(w, m)
			return
		}
		changed := a.changed
		a.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			w.WriteHeader(http.StatusAccepted)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (a *Actions) writeMessage(w http.ResponseWriter, m *message) {
	body, err := json.Marshal(m.body)
	if err != nil {
		a.t.Errorf("fakeactions: %v", err)
	}
	if m.body == nil {
		body = []byte("[]")
	}
	stats := m.stats
	writeJSON(w, http.StatusOK, map[string]any{
		"messageId":   m.id,
		"messageType": "RunnerScaleSetJobMessages",
		"body":        string(body),
		"statistics":  &stats,
	})
}

func (a *Actions) generateJIT(w http.ResponseWriter, r *http.Request, id int) {
	var setting scaleset.RunnerScaleSetJitRunnerSetting
	_ = json.NewDecoder(r.Body).Decode(&setting)
	a.mu.Lock()
	defer a.mu.Unlock()
	ss := a.scaleSets[id]
	if ss == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
		return
	}
	a.logLocked("JIT %s", ss.Name)
	a.nextRunner++
	a.runners[setting.Name] = &registered{id: a.nextRunner, scaleSet: id}
	writeJSON(w, http.StatusOK, scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: a.nextRunner, Name: setting.Name, RunnerScaleSetID: id},
		EncodedJITConfig: "jit-" + setting.Name,
	})
}

func (a *Actions) agents(w http.ResponseWriter, r *http.Request, rawID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Method == http.MethodGet {
		name := r.URL.Query().Get("agentName")
		var found []scaleset.RunnerReference
		if reg := a.runners[name]; reg != nil {
			found = append(found, scaleset.RunnerReference{ID: reg.id, Name: name, RunnerScaleSetID: reg.scaleSet})
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": len(found), "value": found})
		return
	}
	id, _ := strconv.Atoi(rawID)
	for name, reg := range a.runners {
		if reg.id != id {
			continue
		}
		a.logLocked("DELETE runner %s", name)
		if reg.busy {
			writeException(w, http.StatusBadRequest, stillRunningException, "a job is still running")
			return
		}
		delete(a.runners, name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeException(w, http.StatusNotFound, "AgentNotFoundException", "not found")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeException(w http.ResponseWriter, status int, typeName, msg string) {
	writeJSON(w, status, map[string]string{"typeName": typeName, "message": msg})
}
