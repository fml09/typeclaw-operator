package desktopgateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type actorKind string

const (
	actorHuman actorKind = "human"
	actorAgent actorKind = "agent"
)

var (
	errControlBusy           = errors.New("another actor controls this desktop")
	errControlBlocked        = errors.New("desktop control is blocked by its power state")
	errPowerBusy             = errors.New("another power operation is in progress")
	errPowerRecoveryRequired = errors.New("desktop power recovery requires an explicit start")
	errRevocationTimed       = errors.New("the previous controller did not revoke in time")
)

// controller is one granted Input Controller lease.
type controller struct {
	actor      actorKind
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once
	// ttl > 0 marks a lease acquired over HTTP by the agent plugin. Such a
	// lease has no socket lifecycle and must expire when the owner stops
	// refreshing it; RFB controllers never set it.
	ttl       time.Duration
	lastTouch atomic.Int64
}

func (c *controller) closeDone() {
	c.closeOnce.Do(func() { close(c.done) })
}

// controlRegistry arbitrates the exclusive input lease and the power
// quarantine. It is keyed by desktop name even though one process serves one
// desktop, because the lease and quarantine state machines were validated
// under that shape and nothing is gained by flattening them.
type controlRegistry struct {
	mu             sync.Mutex
	active         map[string]*controller
	generations    map[string]uint64
	controlBlocked map[string]bool
	powerOps       map[string]*powerOperation
	takeovers      map[string]*takeoverReservation
	draining       bool
}

type powerOperation struct {
	action       string
	priorBlocked bool
}

type takeoverReservation struct{}

func newControlRegistry() *controlRegistry {
	return &controlRegistry{
		active:         make(map[string]*controller),
		generations:    make(map[string]uint64),
		controlBlocked: make(map[string]bool),
		powerOps:       make(map[string]*powerOperation),
		takeovers:      make(map[string]*takeoverReservation),
	}
}

// acquire grants the socket-scoped lease used by RFB controllers. A human may
// pre-empt an agent lease; nothing else may pre-empt anything.
func (r *controlRegistry) acquire(ctx context.Context, desktop string, actor actorKind, takeover bool) (*controller, error) {
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if r.draining {
		r.mu.Unlock()
		return nil, errControlBlocked
	}
	if r.controlBlocked[desktop] {
		r.mu.Unlock()
		return nil, errControlBlocked
	}
	if r.takeovers[desktop] != nil {
		r.mu.Unlock()
		return nil, errControlBusy
	}
	current := r.active[desktop]
	if current == nil {
		granted := r.grantLocked(ctx, desktop, actor)
		r.mu.Unlock()
		return granted, nil
	}

	canTakeOver := takeover && actor == actorHuman && current.actor == actorAgent
	if !canTakeOver {
		r.mu.Unlock()
		return nil, errControlBusy
	}

	// The reservation holds the desktop across the drain window so a third
	// party cannot slip in between the incumbent's cancel and its release.
	reservation := &takeoverReservation{}
	r.takeovers[desktop] = reservation
	previousDone := current.done
	current.cancel()
	r.mu.Unlock()

	timer := time.NewTimer(3 * time.Second)
	var waitErr error
	select {
	case <-previousDone:
		stopTimer(timer)
	case <-timer.C:
		waitErr = errRevocationTimed
	case <-ctx.Done():
		stopTimer(timer)
		waitErr = ctx.Err()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.takeovers[desktop] != reservation {
		return nil, errControlBusy
	}
	if waitErr == nil {
		waitErr = ctx.Err()
	}
	if waitErr != nil {
		delete(r.takeovers, desktop)
		return nil, waitErr
	}
	if r.controlBlocked[desktop] {
		delete(r.takeovers, desktop)
		return nil, errControlBlocked
	}
	if r.active[desktop] != nil {
		delete(r.takeovers, desktop)
		return nil, errControlBusy
	}
	delete(r.takeovers, desktop)
	return r.grantLocked(ctx, desktop, actor), nil
}

func (r *controlRegistry) grantLocked(ctx context.Context, desktop string, actor actorKind) *controller {
	r.generations[desktop]++
	leaseCtx, cancel := context.WithCancel(ctx)
	granted := &controller{
		actor:      actor,
		generation: r.generations[desktop],
		ctx:        leaseCtx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	r.active[desktop] = granted
	return granted
}

// acquireAgent grants the exclusive control lease to the agent without an RFB
// connection. The lease is anchored to context.Background() because the HTTP
// request that creates it ends immediately; the expire loop below owns its
// lifetime instead.
func (r *controlRegistry) acquireAgent(desktop string, ttl time.Duration) (*controller, error) {
	r.mu.Lock()
	if r.draining || r.controlBlocked[desktop] {
		r.mu.Unlock()
		return nil, errControlBlocked
	}
	if r.takeovers[desktop] != nil || r.active[desktop] != nil {
		r.mu.Unlock()
		return nil, errControlBusy
	}
	granted := r.grantLocked(context.Background(), desktop, actorAgent)
	granted.ttl = ttl
	granted.lastTouch.Store(time.Now().UnixNano())
	r.mu.Unlock()
	go granted.expireLoop(r, desktop)
	return granted, nil
}

func (c *controller) expireLoop(r *controlRegistry, desktop string) {
	interval := c.ttl / 4
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			r.release(desktop, c)
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, c.lastTouch.Load())) > c.ttl {
				r.release(desktop, c)
				return
			}
		}
	}
}

// touchAgent extends the idle deadline of an HTTP agent lease. View-only agent
// calls also count: a model observing or listing windows is still driving the
// session.
func (r *controlRegistry) touchAgent(desktop string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.active[desktop]; current != nil && current.actor == actorAgent && current.ttl > 0 {
		current.lastTouch.Store(time.Now().UnixNano())
	}
}

// releaseAgentOwner releases the desktop only when an agent lease owns it. A
// human controller cannot be released by the agent.
func (r *controlRegistry) releaseAgentOwner(desktop string) bool {
	r.mu.Lock()
	current := r.active[desktop]
	if current == nil || current.actor != actorAgent {
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()
	r.release(desktop, current)
	return true
}

// beginPower reserves the desktop for one power operation and quarantines
// control for its duration. Only an explicit start may run while a previous
// operation left the desktop quarantined.
func (r *controlRegistry) beginPower(desktop, action string) (*powerOperation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return nil, errControlBlocked
	}
	if r.active[desktop] != nil {
		return nil, errControlBusy
	}
	if r.takeovers[desktop] != nil {
		return nil, errControlBusy
	}
	if r.powerOps[desktop] != nil {
		return nil, errPowerBusy
	}
	if r.controlBlocked[desktop] && action != "start" {
		return nil, errPowerRecoveryRequired
	}
	operation := &powerOperation{action: action, priorBlocked: r.controlBlocked[desktop]}
	r.powerOps[desktop] = operation
	r.controlBlocked[desktop] = true
	return operation, nil
}

// finishPower resolves the reservation. A rejected operation definitely did
// not run, so it restores whatever quarantine existed before it and installs
// none of its own; an unknown outcome keeps the quarantine until an explicit
// start succeeds.
func (r *controlRegistry) finishPower(desktop string, operation *powerOperation, outcome powerOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.powerOps[desktop] != operation {
		return
	}
	delete(r.powerOps, desktop)
	if r.draining {
		r.controlBlocked[desktop] = true
		return
	}
	if outcome == powerSucceeded {
		if operation.action == "start" {
			delete(r.controlBlocked, desktop)
		}
		return
	}
	if outcome == powerUnknown {
		return
	}
	if operation.priorBlocked {
		r.controlBlocked[desktop] = true
	} else {
		delete(r.controlBlocked, desktop)
	}
}

// revokeAll drains the registry for shutdown: no new lease or power operation
// is admitted afterwards, and every live controller is cancelled and waited on
// so an RFB peer learns it lost input before the process exits.
func (r *controlRegistry) revokeAll(ctx context.Context) {
	r.mu.Lock()
	r.draining = true
	controllers := make([]*controller, 0, len(r.active))
	for desktop, current := range r.active {
		r.controlBlocked[desktop] = true
		controllers = append(controllers, current)
		current.cancel()
	}
	for desktop := range r.takeovers {
		r.controlBlocked[desktop] = true
		delete(r.takeovers, desktop)
	}
	for desktop := range r.powerOps {
		r.controlBlocked[desktop] = true
	}
	r.mu.Unlock()
	for _, current := range controllers {
		select {
		case <-current.done:
		case <-ctx.Done():
			return
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (r *controlRegistry) release(desktop string, granted *controller) {
	r.mu.Lock()
	if r.active[desktop] == granted {
		delete(r.active, desktop)
	}
	r.mu.Unlock()
	granted.closeDone()
}

func (r *controlRegistry) status(desktop string) (actorKind, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.active[desktop]
	if current == nil {
		return "", r.generations[desktop], false
	}
	return current.actor, current.generation, true
}

func (r *controlRegistry) powerStatus(desktop string) (blocked, active bool, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	operation := r.powerOps[desktop]
	if operation == nil {
		return r.controlBlocked[desktop], false, ""
	}
	return r.controlBlocked[desktop], true, operation.action
}
