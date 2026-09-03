package desktopgateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHumanTakeoverWaitsForAgentRelease(t *testing.T) {
	registry := newControlRegistry()
	agent, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		<-agent.ctx.Done()
		registry.release("pd-a", agent)
		close(released)
	}()

	human, err := registry.acquire(context.Background(), "pd-a", actorHuman, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("human grant activated before the agent grant was released")
	}
	if human.generation <= agent.generation {
		t.Fatalf("generation did not increase: agent=%d human=%d", agent.generation, human.generation)
	}
	if _, err := registry.acquire(context.Background(), "pd-a", actorHuman, true); !errors.Is(err, errControlBusy) {
		t.Fatalf("second human acquire error = %v, want %v", err, errControlBusy)
	}
	registry.release("pd-a", human)
}

func TestHumanTakeoverReservationBlocksInterveningAgent(t *testing.T) {
	registry := newControlRegistry()
	agent, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	type acquireResult struct {
		controller *controller
		err        error
	}
	humanResult := make(chan acquireResult, 1)
	go func() {
		controller, acquireErr := registry.acquire(context.Background(), "pd-a", actorHuman, true)
		humanResult <- acquireResult{controller: controller, err: acquireErr}
	}()
	waitForTakeoverReservation(t, registry, "pd-a")

	// Reproduce the exact release window: active is empty, the original waiter
	// has not observed done yet, and the takeover reservation remains installed.
	registry.mu.Lock()
	delete(registry.active, "pd-a")
	registry.mu.Unlock()
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBusy) {
		t.Fatalf("intervening agent acquire = %v, want %v", err, errControlBusy)
	}
	agent.closeDone()

	select {
	case result := <-humanResult:
		if result.err != nil {
			t.Fatal(result.err)
		}
		registry.release("pd-a", result.controller)
	case <-time.After(time.Second):
		t.Fatal("reserved human takeover did not complete")
	}
}

func TestCanceledTakeoverCannotWinAgainstSimultaneousRelease(t *testing.T) {
	registry := newControlRegistry()
	agent, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, acquireErr := registry.acquire(ctx, "pd-a", actorHuman, true)
		result <- acquireErr
	}()
	waitForTakeoverReservation(t, registry, "pd-a")

	cancel()
	registry.release("pd-a", agent)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("takeover error = %v, want context canceled", err)
	}
	actor, _, active := registry.status("pd-a")
	if active {
		t.Fatalf("canceled takeover left active controller %q", actor)
	}
}

func waitForTakeoverReservation(t *testing.T, registry *controlRegistry, desktop string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		registry.mu.Lock()
		reserved := registry.takeovers[desktop] != nil
		registry.mu.Unlock()
		if reserved {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("human takeover did not reserve the desktop")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPowerReservationBlocksControlUntilStartSucceeds(t *testing.T) {
	registry := newControlRegistry()
	failedStop, err := registry.beginPower("pd-rollback", "stop")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-rollback", failedStop, powerRejected)
	rollbackController, err := registry.acquire(context.Background(), "pd-rollback", actorAgent, false)
	if err != nil {
		t.Fatalf("failed stop did not roll back control block: %v", err)
	}
	registry.release("pd-rollback", rollbackController)

	stop, err := registry.beginPower("pd-a", "stop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire during stop = %v, want %v", err, errControlBlocked)
	}
	registry.finishPower("pd-a", stop, powerSucceeded)
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire after successful stop = %v, want %v", err, errControlBlocked)
	}

	start, err := registry.beginPower("pd-a", "start")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-a", start, powerRejected)
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire after failed start = %v, want %v", err, errControlBlocked)
	}

	start, err = registry.beginPower("pd-a", "start")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-a", start, powerSucceeded)
	controller, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.beginPower("pd-a", "stop"); !errors.Is(err, errControlBusy) {
		t.Fatalf("stop with active control = %v, want %v", err, errControlBusy)
	}
	registry.release("pd-a", controller)
}

func TestDrainRejectsNewControlAndPower(t *testing.T) {
	registry := newControlRegistry()
	registry.revokeAll(context.Background())
	if _, err := registry.acquire(context.Background(), "pd-new", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire while draining = %v, want %v", err, errControlBlocked)
	}
	if _, err := registry.beginPower("pd-new", "start"); !errors.Is(err, errControlBlocked) {
		t.Fatalf("power while draining = %v, want %v", err, errControlBlocked)
	}
}

func TestRevokeAllCancelsLiveControllers(t *testing.T) {
	registry := newControlRegistry()
	granted, err := registry.acquire(context.Background(), "pd-a", actorHuman, false)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-granted.ctx.Done()
		registry.release("pd-a", granted)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	registry.revokeAll(ctx)
	select {
	case <-granted.done:
	default:
		t.Fatal("revokeAll returned before the controller was released")
	}
}

func TestAmbiguousStopKeepsControlBlockedUntilExplicitStartSucceeds(t *testing.T) {
	registry := newControlRegistry()
	stop, err := registry.beginPower("pd-a", "stop")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-a", stop, powerUnknown)
	if _, err := registry.acquire(context.Background(), "pd-a", actorAgent, false); !errors.Is(err, errControlBlocked) {
		t.Fatalf("acquire after ambiguous stop = %v, want %v", err, errControlBlocked)
	}
	if _, err := registry.beginPower("pd-a", "stop"); !errors.Is(err, errPowerRecoveryRequired) {
		t.Fatalf("second stop after ambiguous stop = %v, want %v", err, errPowerRecoveryRequired)
	}
	blocked, active, action := registry.powerStatus("pd-a")
	if !blocked || active || action != "" {
		t.Fatalf("power status after ambiguous stop = (%v, %v, %q), want blocked recovery state", blocked, active, action)
	}

	start, err := registry.beginPower("pd-a", "start")
	if err != nil {
		t.Fatal(err)
	}
	registry.finishPower("pd-a", start, powerSucceeded)
	controller, err := registry.acquire(context.Background(), "pd-a", actorAgent, false)
	if err != nil {
		t.Fatal(err)
	}
	registry.release("pd-a", controller)
}

func TestConcurrentPowerOperationsAreSerialized(t *testing.T) {
	registry := newControlRegistry()
	first, err := registry.beginPower("pd-a", "start")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.beginPower("pd-a", "start"); !errors.Is(err, errPowerBusy) {
		t.Fatalf("second concurrent power operation = %v, want %v", err, errPowerBusy)
	}
	registry.finishPower("pd-a", first, powerSucceeded)
}

func TestAgentLeaseExpiresWithoutTouchAndSurvivesWithTouch(t *testing.T) {
	registry := newControlRegistry()
	if _, err := registry.acquireAgent("pd-expire", 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	renewing, err := registry.acquireAgent("pd-renew", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		registry.touchAgent("pd-renew")
		if _, _, active := registry.status("pd-expire"); !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, active := registry.status("pd-expire"); active {
		t.Fatal("unrefreshed agent lease outlived its TTL")
	}
	if _, _, active := registry.status("pd-renew"); !active {
		t.Fatal("touched agent lease expired despite refreshes")
	}
	registry.mu.Lock()
	current, ok := registry.active["pd-renew"]
	registry.mu.Unlock()
	if !ok || current != renewing {
		t.Fatal("touched agent lease lost its controller identity")
	}
	registry.release("pd-renew", renewing)
	if _, _, active := registry.status("pd-renew"); active {
		t.Fatal("explicit release did not clear the agent lease")
	}
}

func TestAgentReleaseCannotDropHumanController(t *testing.T) {
	registry := newControlRegistry()
	human, err := registry.acquire(context.Background(), "pd-a", actorHuman, false)
	if err != nil {
		t.Fatal(err)
	}
	if registry.releaseAgentOwner("pd-a") {
		t.Fatal("agent release removed a human controller")
	}
	if _, _, active := registry.status("pd-a"); !active {
		t.Fatal("human controller was not active after refused agent release")
	}
	registry.release("pd-a", human)
}
