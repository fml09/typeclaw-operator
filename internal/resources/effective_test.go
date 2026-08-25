package resources

import (
	"testing"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

func TestEffectiveRuntimeImagePromotionWinsWhileActing(t *testing.T) {
	promoted := &typeclawv1alpha1.TypeClawInstance{}
	promoted.Name = "kakao-agent"
	promoted.Namespace = "agents"
	promoted.Status.Update = &typeclawv1alpha1.UpdateStatus{
		PromotedImage: "ghcr.io/fml09/typeclaw-runtime:0.49.0",
	}

	for _, phase := range []typeclawv1alpha1.UpdatePhase{
		typeclawv1alpha1.UpdatePhaseUpdating,
		typeclawv1alpha1.UpdatePhaseConfirming,
		typeclawv1alpha1.UpdatePhaseReady,
	} {
		promoted.Status.Update.Phase = phase
		if got := EffectiveRuntimeImage(promoted); got != "ghcr.io/fml09/typeclaw-runtime:0.49.0" {
			t.Fatalf("phase %v: promoted image must win during rollout, got %q", phase, got)
		}
	}
}

func TestEffectiveRuntimeImageFallsBackToSpecResolution(t *testing.T) {
	in := instance("kakao-agent", nil)

	cases := []struct {
		name   string
		status *typeclawv1alpha1.UpdateStatus
	}{
		{name: "nil update status"},
		{
			name:   "idle keeps spec resolution",
			status: &typeclawv1alpha1.UpdateStatus{Phase: typeclawv1alpha1.UpdatePhaseIdle, PromotedImage: "ghcr.io/fml09/typeclaw-runtime:0.49.0"},
		},
		{
			name:   "awaiting backup is not acting",
			status: &typeclawv1alpha1.UpdateStatus{Phase: typeclawv1alpha1.UpdatePhaseAwaitingBackup, PromotedImage: "ghcr.io/fml09/typeclaw-runtime:0.49.0"},
		},
		{
			name:   "rolled back returns to spec",
			status: &typeclawv1alpha1.UpdateStatus{Phase: typeclawv1alpha1.UpdatePhaseRolledBack, PromotedImage: "ghcr.io/fml09/typeclaw-runtime:0.49.0"},
		},
		{
			name:   "acting without promoted image",
			status: &typeclawv1alpha1.UpdateStatus{Phase: typeclawv1alpha1.UpdatePhaseUpdating},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in.Status.Update = tc.status
			want := "ghcr.io/fml09/typeclaw-runtime:" + typeclawv1alpha1.DefaultRuntimeVersion
			if got := EffectiveRuntimeImage(in); got != want {
				t.Fatalf("spec-derived image expected %q, got %q", want, got)
			}
		})
	}
}
