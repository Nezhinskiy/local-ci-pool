package supervisor

import "context"

// Each project's listener advertises a capacity to GitHub with every message
// request (the scale set's maximum runners), and GitHub assigns the scale set
// no more jobs than that. The slots are shared by every project, so a fixed
// capacity of all the slots let GitHub assign each project jobs the Mac had
// no slot for; an assigned job no runner acquires within 5 minutes is
// completed as canceled and reissued. The capacity is therefore the slots the
// project could use now: the free slots, plus its own runners and survivors,
// whose jobs are already assigned to it. It is advertised again whenever a
// slot is taken or given back, and is 0 while the project drains. A listener
// reads it when it sends its next message request, so a change reaches GitHub
// within one long poll (about 50 s).

// requestCapacity asks capacities to advertise every capacity again. It never
// blocks, so the slots' OnChange may call it under a Scaler's lock.
func (s *Supervisor) requestCapacity() {
	select {
	case s.capacity <- struct{}{}:
	default:
	}
}

// capacities advertises every project's capacity on each request, until life
// ends.
func (s *Supervisor) capacities(life context.Context) {
	for {
		select {
		case <-life.Done():
			return
		case <-s.capacity:
		}
		s.mu.Lock()
		prs := s.projectList()
		s.mu.Unlock()
		for _, pr := range prs {
			pr.advertise()
		}
	}
}

// capacity is the slots pr could use now: the free slots plus its runners and
// survivors, at most every slot. It takes the supervisor's and the scaler's
// locks: never call it under pr.mu.
func (pr *project) capacity() int {
	s := pr.s
	own := s.survivorsOf(pr.identity)
	if sc := pr.getScaler(); sc != nil {
		own += len(sc.Held())
	}
	return min(max(s.slots.Cap()-s.slots.InUse(), 0)+own, s.slots.Cap())
}

// advertise gives pr's listener its capacity, 0 while it drains.
func (pr *project) advertise() {
	c := pr.capacity()
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.listener == nil {
		return
	}
	if pr.draining {
		c = 0
	}
	if c != pr.advertised {
		pr.log.Debug("advertising the scale set's capacity", "max_runners", c)
		pr.advertised = c
	}
	pr.listener.SetMaxRunners(c)
}
