package desktopgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// powerOutcome is the gateway's internal classification of a power dispatch.
type powerOutcome int

const (
	powerSucceeded powerOutcome = iota
	powerRejected
	powerUnknown
)

// Outcome labels carried in every power response body. Clients read these
// before the HTTP status: the status says how the request went, the outcome
// says what happened to the desktop (ticket #20).
const (
	outcomeSucceeded = "Succeeded"
	outcomeRejected  = "Rejected"
	outcomeUnknown   = "UnknownOutcome"
)

func (o powerOutcome) label() string {
	switch o {
	case powerSucceeded:
		return outcomeSucceeded
	case powerRejected:
		return outcomeRejected
	default:
		return outcomeUnknown
	}
}

// powerResult is the explicit result of one KubeVirt power dispatch. Rejected
// means the operation provably did not run, so control is not quarantined and
// the caller may act on the failure; UnknownOutcome means the desktop may have
// moved, so control stays quarantined until an explicit start succeeds.
type powerResult struct {
	outcome    powerOutcome
	idempotent bool
	err        error
}

func (g *Gateway) handlePower(authenticate authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := authenticate(w, r)
		if !ok {
			return
		}
		if !g.requireMutationOrigin(w, r, id) {
			return
		}
		action := r.PathValue("action")
		if action != "start" && action != "stop" {
			writeError(w, http.StatusNotFound, "unknown power action", nil)
			return
		}
		operation, err := g.controls.beginPower(g.config.Name, action)
		if err != nil {
			if errors.Is(err, errPowerRecoveryRequired) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":          "desktop power recovery requires an explicit start",
					"detail":         err.Error(),
					"desktopName":    g.config.Name,
					"action":         action,
					"outcome":        outcomeRejected,
					"retrySafe":      false,
					"controlBlocked": true,
					"recoveryAction": "start",
				})
				return
			}
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "power operation unavailable",
				"detail":      err.Error(),
				"desktopName": g.config.Name,
				"action":      action,
				"outcome":     outcomeRejected,
				"retrySafe":   true,
			})
			return
		}

		powerCtx, cancelPower := context.WithTimeout(r.Context(), g.effectivePowerTimeout())
		defer cancelPower()
		result := g.dispatchPower(powerCtx, action)
		g.controls.finishPower(g.config.Name, operation, result.outcome)
		controlBlocked, _, _ := g.controls.powerStatus(g.config.Name)
		if g.logger != nil {
			g.logger.Info("desktop power operation finished",
				"desktop", g.config.Name, "action", action,
				"outcome", result.outcome.label(), "idempotent", result.idempotent,
				"controlBlocked", controlBlocked)
		}

		switch result.outcome {
		case powerSucceeded:
			writeJSON(w, http.StatusAccepted, map[string]any{
				"desktopName": g.config.Name,
				"action":      action,
				"outcome":     outcomeSucceeded,
				"idempotent":  result.idempotent,
			})
		case powerRejected:
			writeJSON(w, kubeVirtOperationStatus(result.err), map[string]any{
				"error":          "KubeVirt power operation failed",
				"detail":         result.err.Error(),
				"desktopName":    g.config.Name,
				"action":         action,
				"outcome":        outcomeRejected,
				"retrySafe":      true,
				"controlBlocked": controlBlocked,
			})
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":          "KubeVirt power outcome is unknown; desktop control remains blocked",
				"detail":         result.err.Error(),
				"desktopName":    g.config.Name,
				"action":         action,
				"outcome":        outcomeUnknown,
				"retrySafe":      false,
				"controlBlocked": true,
			})
		}
	}
}

// dispatchPower runs one power verb and classifies what it means. A conflict
// is the one answer KubeVirt gives for both "you asked for the state it is
// already in" and "it is mid-transition", so a conflict is resolved by reading
// the settled state rather than by assuming either.
func (g *Gateway) dispatchPower(ctx context.Context, action string) powerResult {
	var err error
	switch action {
	case "start":
		err = g.kubevirt.Start(ctx, g.config.Namespace, g.config.Name)
	case "stop":
		err = g.kubevirt.Stop(ctx, g.config.Namespace, g.config.Name)
	default:
		return powerResult{outcome: powerRejected, err: fmt.Errorf("unknown power action %q", action)}
	}
	if err == nil {
		return powerResult{outcome: powerSucceeded}
	}
	if apierrors.IsConflict(err) {
		settled, confirmErr := g.confirmSettledAfterConflict(ctx, action)
		switch {
		case confirmErr != nil:
			// The conflict can only be resolved by the observed state, and
			// that state is unreadable. Reporting the read failure as a
			// rejection would invite a retry against a desktop that may
			// already have moved.
			return powerResult{outcome: powerUnknown, err: confirmErr}
		case settled:
			return powerResult{outcome: powerSucceeded, idempotent: true}
		}
	}
	if definitivePowerRejection(err) {
		return powerResult{outcome: powerRejected, err: err}
	}
	return powerResult{outcome: powerUnknown, err: err}
}

// confirmSettledAfterConflict reports whether the desktop is already stably in
// the state the caller asked for, which makes the conflict an idempotent
// success rather than a failure.
func (g *Gateway) confirmSettledAfterConflict(ctx context.Context, action string) (bool, error) {
	vm, err := g.kubevirt.GetVM(ctx, g.config.Namespace, g.config.Name)
	if err != nil {
		return false, fmt.Errorf("confirm VirtualMachine after %s conflict: %w", action, err)
	}
	vmi, err := g.kubevirt.GetVMI(ctx, g.config.Namespace, g.config.Name)
	switch {
	case err == nil:
	case action == "stop" && apierrors.IsNotFound(err):
		// A stopped desktop has no VirtualMachineInstance; its absence is the
		// evidence the stop asked for, not a failed read.
		vmi = nil
	default:
		return false, fmt.Errorf("confirm VirtualMachineInstance after %s conflict: %w", action, err)
	}
	if action == "start" {
		return stableRunningAfterStartConflict(vm, vmi), nil
	}
	return stableStoppedAfterStopConflict(vm, vmi), nil
}

// stableRunningAfterStartConflict recognises the one start conflict that is
// really a success: the desktop is already Running with nothing pending. A
// queued state change means the observed status has not caught up with an
// intent, so it cannot settle the conflict.
func stableRunningAfterStartConflict(vm *VirtualMachine, vmi *VirtualMachineInstance) bool {
	return vm != nil &&
		!vm.Deleting &&
		vm.PrintableStatus == VirtualMachineStatusRunning &&
		len(vm.StateChangeRequests) == 0 &&
		vmi != nil &&
		vmi.Phase == VirtualMachineInstanceRunning &&
		!vmi.Deleting
}

// stableStoppedAfterStopConflict is the stop-direction mirror: a Manual VM
// that is already Stopped, has nothing queued, and has no running
// VirtualMachineInstance left. The run strategy is checked because only a
// Manual VM is driven exclusively through these subresources; under any other
// strategy something else may be about to start it, and "Stopped" is then not
// a settled state.
//
// A final-phase instance counts as none. A guest that shut itself down leaves
// its Succeeded or Failed VirtualMachineInstance behind under runStrategy
// Manual, and KubeVirt answers the owner's next stop with the same Conflict it
// gives for an absent instance; requiring the object to be gone would classify
// that already-stopped desktop as UnknownOutcome and quarantine control until
// someone issued an explicit start.
func stableStoppedAfterStopConflict(vm *VirtualMachine, vmi *VirtualMachineInstance) bool {
	return vm != nil &&
		!vm.Deleting &&
		vm.RunStrategy == RunStrategyManual &&
		vm.PrintableStatus == VirtualMachineStatusStopped &&
		len(vm.StateChangeRequests) == 0 &&
		(vmi == nil || finalInstancePhase(vmi.Phase))
}

// definitivePowerRejection reports whether the API server answered in a way
// that proves the operation did not and will not take effect. Only these
// reasons may clear a power reservation without quarantining control; a
// timeout or a transport failure never can, because the request may still be
// in flight.
func definitivePowerRejection(err error) bool {
	switch apierrors.ReasonForError(err) {
	case metav1.StatusReasonNotFound,
		metav1.StatusReasonUnauthorized,
		metav1.StatusReasonForbidden,
		metav1.StatusReasonBadRequest,
		metav1.StatusReasonInvalid,
		metav1.StatusReasonMethodNotAllowed,
		metav1.StatusReasonNotAcceptable,
		metav1.StatusReasonRequestEntityTooLarge,
		metav1.StatusReasonUnsupportedMediaType:
		return true
	default:
		return false
	}
}

// kubeVirtOperationStatus maps a rejected power operation onto the status the
// caller should see. Every definite rejection answers 4xx, because a client
// that classifies on the HTTP status alone reads 5xx as "the desktop may have
// moved" and then refuses to retry an operation that provably never ran.
// Authorization failures answer 409 rather than 401 or 403: they are the
// gateway ServiceAccount's own RBAC failing, and 403 would tell a caller to
// re-authenticate against a boundary it already passed.
func kubeVirtOperationStatus(err error) int {
	switch {
	case apierrors.IsNotFound(err):
		return http.StatusNotFound
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		return http.StatusBadRequest
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsServiceUnavailable(err):
		return http.StatusServiceUnavailable
	case apierrors.IsConflict(err), definitivePowerRejection(err):
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}
