package resources

import (
	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// EffectiveRuntimeImage returns the image the Managed Runtime should run
// right now: an active promotion wins over spec-derived resolution.
//
// StatefulSet rendering calls this so a rollout promoted by the auto-update
// controller survives reconcile loops without rewriting spec.runtime;
// GitOps-authored specs stay authoritative about intent while status.update
// tracks what actually runs (ADR 0004). It lives in this package because the
// update controller already imports resources — inverting the dependency
// would create a cycle.
func EffectiveRuntimeImage(in *typeclawv1alpha1.TypeClawInstance) string {
	if u := in.Status.Update; u != nil && u.PromotedImage != "" &&
		(u.Phase == typeclawv1alpha1.UpdatePhaseUpdating ||
			u.Phase == typeclawv1alpha1.UpdatePhaseConfirming ||
			u.Phase == typeclawv1alpha1.UpdatePhaseReady) {
		return u.PromotedImage
	}
	return ResolveRuntimeImage(in.Spec)
}
