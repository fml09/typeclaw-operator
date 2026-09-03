package desktopgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func runningVM() *VirtualMachine {
	return &VirtualMachine{
		UID:             "vm-a",
		RunStrategy:     RunStrategyManual,
		PrintableStatus: VirtualMachineStatusRunning,
	}
}

func stoppedVM() *VirtualMachine {
	return &VirtualMachine{
		UID:             "vm-a",
		RunStrategy:     RunStrategyManual,
		PrintableStatus: VirtualMachineStatusStopped,
	}
}

func runningVMI() *VirtualMachineInstance {
	return &VirtualMachineInstance{UID: "vmi-a", Phase: VirtualMachineInstanceRunning}
}

// finalVMI is the object a Manual desktop leaves behind when the guest shuts
// itself down: the instance stays until the next start, in a phase it can
// never leave.
func finalVMI(phase string) *VirtualMachineInstance {
	return &VirtualMachineInstance{UID: "vmi-a", Phase: phase}
}

func conflict(detail string) error {
	return apierrors.NewConflict(vmResource, testName, errors.New(detail))
}

func powerRequest(t *testing.T, action string) *http.Request {
	t.Helper()
	request := agentRequest(t, http.MethodPost, "https://gateway.svc/api/power/"+action, nil)
	request.SetPathValue("action", action)
	return request
}

func decodeBody(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	return body
}

// Ticket #20: every power answer states an outcome, a definite rejection never
// installs a quarantine, and only an ambiguous one keeps control blocked.
func TestPowerOutcomesAreExplicit(t *testing.T) {
	tests := []struct {
		name           string
		action         string
		kubevirt       *fakeKubeVirt
		wantStatus     int
		wantOutcome    string
		wantIdempotent bool
		wantBlocked    bool
	}{
		{
			name:        "start succeeds",
			action:      "start",
			kubevirt:    &fakeKubeVirt{},
			wantStatus:  http.StatusAccepted,
			wantOutcome: outcomeSucceeded,
		},
		{
			name:   "stop succeeds and keeps control blocked until a start",
			action: "stop",
			kubevirt: &fakeKubeVirt{
				stop: func(context.Context, string, string) error { return nil },
			},
			wantStatus:  http.StatusAccepted,
			wantOutcome: outcomeSucceeded,
			wantBlocked: true,
		},
		{
			name:   "forbidden start is rejected without a quarantine",
			action: "start",
			kubevirt: &fakeKubeVirt{
				start: func(context.Context, string, string) error {
					return apierrors.NewForbidden(vmResource, testName, errors.New("virtualmachines/start is forbidden"))
				},
			},
			wantStatus:  http.StatusConflict,
			wantOutcome: outcomeRejected,
		},
		{
			name:   "bad request stop is rejected without a quarantine",
			action: "stop",
			kubevirt: &fakeKubeVirt{
				stop: func(context.Context, string, string) error {
					return apierrors.NewBadRequest("gracePeriod must not be negative")
				},
			},
			wantStatus:  http.StatusBadRequest,
			wantOutcome: outcomeRejected,
		},
		{
			name:   "missing VM start is rejected without a quarantine",
			action: "start",
			kubevirt: &fakeKubeVirt{
				start: func(context.Context, string, string) error { return vmNotFound() },
			},
			wantStatus:  http.StatusNotFound,
			wantOutcome: outcomeRejected,
		},
		{
			name:   "start conflict on a stably running desktop is an idempotent success",
			action: "start",
			kubevirt: &fakeKubeVirt{
				start:  func(context.Context, string, string) error { return conflict("VM is already running") },
				getVM:  func(context.Context, string, string) (*VirtualMachine, error) { return runningVM(), nil },
				getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
			},
			wantStatus:     http.StatusAccepted,
			wantOutcome:    outcomeSucceeded,
			wantIdempotent: true,
		},
		{
			name:   "stop conflict on a stably stopped Manual VM is an idempotent success",
			action: "stop",
			kubevirt: &fakeKubeVirt{
				stop:  func(context.Context, string, string) error { return conflict("VM is not running") },
				getVM: func(context.Context, string, string) (*VirtualMachine, error) { return stoppedVM(), nil },
			},
			wantStatus:     http.StatusAccepted,
			wantOutcome:    outcomeSucceeded,
			wantIdempotent: true,
			wantBlocked:    true,
		},
		{
			name:   "stop conflict on a Manual VM whose guest shut itself down is an idempotent success",
			action: "stop",
			kubevirt: &fakeKubeVirt{
				stop:  func(context.Context, string, string) error { return conflict("VM is not running") },
				getVM: func(context.Context, string, string) (*VirtualMachine, error) { return stoppedVM(), nil },
				getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) {
					return finalVMI(VirtualMachineInstanceSucceeded), nil
				},
			},
			wantStatus:     http.StatusAccepted,
			wantOutcome:    outcomeSucceeded,
			wantIdempotent: true,
			wantBlocked:    true,
		},
		{
			name:   "stop conflict with a surviving VMI stays unknown",
			action: "stop",
			kubevirt: &fakeKubeVirt{
				stop:   func(context.Context, string, string) error { return conflict("VM is not running") },
				getVM:  func(context.Context, string, string) (*VirtualMachine, error) { return stoppedVM(), nil },
				getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantOutcome: outcomeUnknown,
			wantBlocked: true,
		},
		{
			name:   "stop conflict on a non-Manual VM stays unknown",
			action: "stop",
			kubevirt: &fakeKubeVirt{
				stop: func(context.Context, string, string) error { return conflict("VM is not running") },
				getVM: func(context.Context, string, string) (*VirtualMachine, error) {
					vm := stoppedVM()
					vm.RunStrategy = "Always"
					return vm, nil
				},
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantOutcome: outcomeUnknown,
			wantBlocked: true,
		},
		{
			name:   "start conflict with a pending stop request stays unknown",
			action: "start",
			kubevirt: &fakeKubeVirt{
				start: func(context.Context, string, string) error { return conflict("VM is already running") },
				getVM: func(context.Context, string, string) (*VirtualMachine, error) {
					vm := runningVM()
					vm.StateChangeRequests = []StateChangeRequest{{Action: "Stop"}}
					return vm, nil
				},
				getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantOutcome: outcomeUnknown,
			wantBlocked: true,
		},
		{
			name:   "start conflict on a deleting VMI stays unknown",
			action: "start",
			kubevirt: &fakeKubeVirt{
				start: func(context.Context, string, string) error { return conflict("VM is already running") },
				getVM: func(context.Context, string, string) (*VirtualMachine, error) { return runningVM(), nil },
				getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) {
					vmi := runningVMI()
					vmi.Deleting = true
					return vmi, nil
				},
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantOutcome: outcomeUnknown,
			wantBlocked: true,
		},
		{
			name:   "start conflict with an unreadable VM stays unknown",
			action: "start",
			kubevirt: &fakeKubeVirt{
				start: func(context.Context, string, string) error { return conflict("VM is already running") },
				getVM: func(context.Context, string, string) (*VirtualMachine, error) {
					return nil, apierrors.NewServiceUnavailable("etcd is down")
				},
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantOutcome: outcomeUnknown,
			wantBlocked: true,
		},
		{
			name:   "start conflict with no VMI yet stays unknown",
			action: "start",
			kubevirt: &fakeKubeVirt{
				start: func(context.Context, string, string) error { return conflict("VM is already running") },
				getVM: func(context.Context, string, string) (*VirtualMachine, error) { return runningVM(), nil },
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantOutcome: outcomeUnknown,
			wantBlocked: true,
		},
		{
			name:   "lost acknowledgement stays unknown",
			action: "stop",
			kubevirt: &fakeKubeVirt{
				stop: func(context.Context, string, string) error {
					return apierrors.NewTimeoutError("lost ACK", 1)
				},
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantOutcome: outcomeUnknown,
			wantBlocked: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := newTestGateway(t, testConfig(), test.kubevirt)
			response := httptest.NewRecorder()
			g.handlePower(g.agentIdentity)(response, powerRequest(t, test.action))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", response.Code, test.wantStatus, response.Body.String())
			}
			body := decodeBody(t, response)
			if body["outcome"] != test.wantOutcome {
				t.Fatalf("outcome = %v, want %q", body["outcome"], test.wantOutcome)
			}
			if test.wantOutcome == outcomeSucceeded && body["idempotent"] != test.wantIdempotent {
				t.Fatalf("idempotent = %v, want %v", body["idempotent"], test.wantIdempotent)
			}
			if test.wantOutcome == outcomeUnknown {
				if body["retrySafe"] != false || body["controlBlocked"] != true {
					t.Fatalf("ambiguous body = %#v", body)
				}
			}
			blocked, active, _ := g.controls.powerStatus(testName)
			if active {
				t.Fatal("the power reservation was not released")
			}
			if blocked != test.wantBlocked {
				t.Fatalf("controlBlocked = %v, want %v", blocked, test.wantBlocked)
			}
		})
	}
}

func TestStartConflictIsIdempotentOnlyForStableRunningVM(t *testing.T) {
	vm := runningVM()
	vmi := runningVMI()
	if !stableRunningAfterStartConflict(vm, vmi) {
		t.Fatal("stable Running VM/VMI was not recognized as an idempotent start")
	}

	vm.StateChangeRequests = []StateChangeRequest{{Action: "Stop"}}
	if stableRunningAfterStartConflict(vm, vmi) {
		t.Fatal("Running VMI with a pending Stop request was treated as stable")
	}
	vm.StateChangeRequests = nil
	vmi.Deleting = true
	if stableRunningAfterStartConflict(vm, vmi) {
		t.Fatal("deleting Running VMI was treated as stable")
	}
	vmi.Deleting = false
	vm.Deleting = true
	if stableRunningAfterStartConflict(vm, vmi) {
		t.Fatal("deleting VM was treated as stable")
	}
	if stableRunningAfterStartConflict(runningVM(), nil) {
		t.Fatal("a missing VMI was treated as a running desktop")
	}
}

func TestStopConflictIsIdempotentOnlyForStableStoppedManualVM(t *testing.T) {
	if !stableStoppedAfterStopConflict(stoppedVM(), nil) {
		t.Fatal("stable Stopped Manual VM was not recognized as an idempotent stop")
	}
	if stableStoppedAfterStopConflict(stoppedVM(), runningVMI()) {
		t.Fatal("a surviving VMI was treated as a stopped desktop")
	}
	for _, phase := range []string{VirtualMachineInstanceSucceeded, VirtualMachineInstanceFailed} {
		if !stableStoppedAfterStopConflict(stoppedVM(), finalVMI(phase)) {
			t.Fatalf("a lingering %s VMI was not recognized as a stopped desktop", phase)
		}
	}
	for _, phase := range []string{"Pending", "Scheduling", "Scheduled", "Unknown"} {
		if stableStoppedAfterStopConflict(stoppedVM(), finalVMI(phase)) {
			t.Fatalf("a %s VMI was treated as a settled stop", phase)
		}
	}

	pending := stoppedVM()
	pending.StateChangeRequests = []StateChangeRequest{{Action: "Start"}}
	if stableStoppedAfterStopConflict(pending, nil) {
		t.Fatal("Stopped VM with a pending Start request was treated as stable")
	}

	running := stoppedVM()
	running.PrintableStatus = VirtualMachineStatusRunning
	if stableStoppedAfterStopConflict(running, nil) {
		t.Fatal("a Running VM was treated as stopped")
	}

	deleting := stoppedVM()
	deleting.Deleting = true
	if stableStoppedAfterStopConflict(deleting, nil) {
		t.Fatal("a deleting VM was treated as stable")
	}

	automatic := stoppedVM()
	automatic.RunStrategy = "Always"
	if stableStoppedAfterStopConflict(automatic, nil) {
		t.Fatal("a non-Manual VM was treated as settled")
	}

	if stableStoppedAfterStopConflict(nil, nil) {
		t.Fatal("a missing VM was treated as stopped")
	}
}

func TestDefinitivePowerRejectionCoversOnlyProvenFailures(t *testing.T) {
	rejections := []error{
		vmNotFound(),
		apierrors.NewForbidden(vmResource, testName, errors.New("no")),
		apierrors.NewUnauthorized("no token"),
		apierrors.NewBadRequest("bad"),
		apierrors.NewMethodNotSupported(vmResource, "update"),
	}
	for _, err := range rejections {
		if !definitivePowerRejection(err) {
			t.Fatalf("%v must be a definitive rejection", err)
		}
	}
	ambiguous := []error{
		apierrors.NewTimeoutError("lost ACK", 1),
		apierrors.NewServiceUnavailable("etcd is down"),
		conflict("busy"),
		errors.New("transport failed"),
		context.DeadlineExceeded,
	}
	for _, err := range ambiguous {
		if definitivePowerRejection(err) {
			t.Fatalf("%v must retain UnknownOutcome semantics", err)
		}
	}
}

func TestKubeVirtOperationStatusPreservesCallerVisibleSemantics(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "missing VM", err: vmNotFound(), want: http.StatusNotFound},
		{name: "lifecycle conflict", err: conflict("busy"), want: http.StatusConflict},
		{name: "bad request", err: apierrors.NewBadRequest("bad"), want: http.StatusBadRequest},
		{name: "service unavailable", err: apierrors.NewServiceUnavailable("down"), want: http.StatusServiceUnavailable},
		{
			name: "gateway RBAC failure is definite but not the caller's to re-authenticate",
			err:  apierrors.NewForbidden(vmResource, testName, errors.New("no")),
			want: http.StatusConflict,
		},
		{name: "unauthorized", err: apierrors.NewUnauthorized("no token"), want: http.StatusConflict},
		{
			name: "method not supported",
			err:  apierrors.NewMethodNotSupported(vmResource, "update"),
			want: http.StatusConflict,
		},
		{name: "upstream failure", err: errors.New("transport failed"), want: http.StatusBadGateway},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := kubeVirtOperationStatus(test.err); got != test.want {
				t.Fatalf("kubeVirtOperationStatus() = %d, want %d", got, test.want)
			}
		})
	}

	// Contract rule: a Rejected outcome always carries a 4xx, so a client that
	// classifies on the status code still reads it as "this never ran".
	definite := []error{
		vmNotFound(),
		apierrors.NewForbidden(vmResource, testName, errors.New("no")),
		apierrors.NewUnauthorized("no token"),
		apierrors.NewBadRequest("bad"),
		apierrors.NewInvalid(schema.GroupKind{Group: vmResource.Group, Kind: "VirtualMachine"}, testName, nil),
		apierrors.NewMethodNotSupported(vmResource, "update"),
		apierrors.NewRequestEntityTooLargeError("too big"),
	}
	for _, err := range definite {
		if status := kubeVirtOperationStatus(err); status < 400 || status > 499 {
			t.Fatalf("definite rejection %v answered %d, want a 4xx", err, status)
		}
	}
}

func TestQuarantinedDesktopAdmitsOnlyAnExplicitStart(t *testing.T) {
	kubevirt := &fakeKubeVirt{}
	g := newTestGateway(t, testConfig(), kubevirt)
	stop, err := g.controls.beginPower(testName, "stop")
	if err != nil {
		t.Fatal(err)
	}
	g.controls.finishPower(testName, stop, powerUnknown)

	response := httptest.NewRecorder()
	g.handlePower(g.agentIdentity)(response, powerRequest(t, "stop"))
	if response.Code != http.StatusConflict {
		t.Fatalf("quarantined stop status = %d, want 409", response.Code)
	}
	body := decodeBody(t, response)
	if body["recoveryAction"] != "start" || body["controlBlocked"] != true || body["retrySafe"] != false {
		t.Fatalf("quarantined stop body = %#v", body)
	}
	if kubevirt.callCount("Stop") != 0 {
		t.Fatal("a quarantined stop reached KubeVirt")
	}

	response = httptest.NewRecorder()
	g.handlePower(g.agentIdentity)(response, powerRequest(t, "start"))
	if response.Code != http.StatusAccepted {
		t.Fatalf("recovery start status = %d, body %s", response.Code, response.Body.String())
	}
	if blocked, _, _ := g.controls.powerStatus(testName); blocked {
		t.Fatal("a successful start did not clear the quarantine")
	}
}

// A pending stop request means the observed state has not settled, so the
// conflict cannot be read as "already running" and the quarantine must hold.
func TestPendingStopConflictCannotClearPowerQuarantine(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		start: func(context.Context, string, string) error { return conflict("VM is already running") },
		getVM: func(context.Context, string, string) (*VirtualMachine, error) {
			vm := runningVM()
			vm.StateChangeRequests = []StateChangeRequest{{Action: "Stop"}}
			return vm, nil
		},
		getVMI: func(context.Context, string, string) (*VirtualMachineInstance, error) { return runningVMI(), nil },
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	stop, err := g.controls.beginPower(testName, "stop")
	if err != nil {
		t.Fatal(err)
	}
	g.controls.finishPower(testName, stop, powerUnknown)

	response := httptest.NewRecorder()
	g.handlePower(g.agentIdentity)(response, powerRequest(t, "start"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("pending-stop start status = %d, want 503 UnknownOutcome", response.Code)
	}
	body := decodeBody(t, response)
	if body["outcome"] != outcomeUnknown || body["controlBlocked"] != true {
		t.Fatalf("pending-stop start response = %#v", body)
	}
	blocked, active, _ := g.controls.powerStatus(testName)
	if !blocked || active {
		t.Fatalf("pending-stop start power status = (%v, %v), want quarantined", blocked, active)
	}
}

func TestPowerHandlerBoundsKubeVirtCallAndReleasesReservation(t *testing.T) {
	kubevirt := &fakeKubeVirt{
		stop: func(ctx context.Context, _, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	g := newTestGateway(t, testConfig(), kubevirt)
	g.powerTimeout = 20 * time.Millisecond

	response := httptest.NewRecorder()
	started := time.Now()
	g.handlePower(g.agentIdentity)(response, powerRequest(t, "stop"))
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded power request took %v, want less than one second", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out power status = %d, want 503", response.Code)
	}
	body := decodeBody(t, response)
	if body["outcome"] != outcomeUnknown || body["retrySafe"] != false || body["controlBlocked"] != true {
		t.Fatalf("timed-out power response = %#v", body)
	}

	blocked, active, _ := g.controls.powerStatus(testName)
	if !blocked || active {
		t.Fatalf("power status after timeout = (%v, %v), want blocked without active reservation", blocked, active)
	}
	if _, err := g.controls.beginPower(testName, "stop"); !errors.Is(err, errPowerRecoveryRequired) {
		t.Fatalf("stop after timeout = %v, want %v", err, errPowerRecoveryRequired)
	}
	start, err := g.controls.beginPower(testName, "start")
	if err != nil {
		t.Fatalf("explicit start recovery was not allowed: %v", err)
	}
	g.controls.finishPower(testName, start, powerSucceeded)
}

func TestPowerRejectsUnknownActionsAndForeignOrigins(t *testing.T) {
	g := newTestGateway(t, testConfig(), &fakeKubeVirt{})

	response := httptest.NewRecorder()
	request := agentRequest(t, http.MethodPost, "https://gateway.svc/api/power/reboot", nil)
	request.SetPathValue("action", "reboot")
	g.handlePower(g.agentIdentity)(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown power action status = %d, want 404", response.Code)
	}

	response = httptest.NewRecorder()
	hostile := powerRequest(t, "stop")
	hostile.Header.Set("Origin", "https://hostile.example")
	g.handlePower(g.agentIdentity)(response, hostile)
	if response.Code != http.StatusForbidden {
		t.Fatalf("hostile origin status = %d, want 403", response.Code)
	}
}

func TestConsolePowerRequiresHumanIdentityAndOrigin(t *testing.T) {
	g := newTestGateway(t, testConfig(), &fakeKubeVirt{})

	response := httptest.NewRecorder()
	request := withOrigin(consoleRequest(t, http.MethodPost, "https://desktop.example/api/power/start"))
	request.SetPathValue("action", "start")
	g.handlePower(g.humanIdentity)(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("console start status = %d, body %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	originless := consoleRequest(t, http.MethodPost, "https://desktop.example/api/power/stop")
	originless.SetPathValue("action", "stop")
	g.handlePower(g.humanIdentity)(response, originless)
	if response.Code != http.StatusForbidden {
		t.Fatalf("originless console stop status = %d, want 403", response.Code)
	}
}
