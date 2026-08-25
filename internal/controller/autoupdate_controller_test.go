package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/resources"
	"github.com/fml09/typeclaw-operator/internal/update"
)

// autoTagsFake serves the GHCR anonymous flow from an httptest server so
// controller tests pin exact candidate selection without network access.
func autoTagsFake(t *testing.T, tags []string) *update.RegistryClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case "/v2/fml09/typeclaw-runtime/tags/list":
			_ = json.NewEncoder(w).Encode(map[string][]string{"tags": tags})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return &update.RegistryClient{BaseURL: server.URL, TokenURL: server.URL + "/token"}
}

func autoReconcile(t *testing.T, r *AutoUpdateReconciler, key types.NamespacedName) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
}

func autoReconcilerFor(t *testing.T, registry *update.RegistryClient, objs ...client.Object) (*AutoUpdateReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&typeclawv1alpha1.TypeClawInstance{}).
		WithObjects(objs...).
		Build()
	return &AutoUpdateReconciler{Client: c, Scheme: c.Scheme(), Registry: registry}, c
}

func autoKey(in *typeclawv1alpha1.TypeClawInstance) types.NamespacedName {
	return types.NamespacedName{Namespace: in.Namespace, Name: in.Name}
}

// autoWorkload seeds the workload StatefulSet rendered for the Instance,
// optionally reporting readiness as a live Pod would.
func autoWorkload(t *testing.T, in *typeclawv1alpha1.TypeClawInstance, readyReplicas int32) *appsv1.StatefulSet {
	t.Helper()
	sts, err := resources.StatefulSet(in)
	if err != nil {
		t.Fatalf("render StatefulSet: %v", err)
	}
	sts.Status.ReadyReplicas = readyReplicas
	return sts
}

func autoRuntimeImage(t *testing.T, c client.Client, key types.NamespacedName) string {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), key, &sts); err != nil {
		t.Fatalf("read StatefulSet: %v", err)
	}
	for _, container := range sts.Spec.Template.Spec.Containers {
		if container.Name == RuntimeContainerName {
			return container.Image
		}
	}
	t.Fatalf("StatefulSet has no %q container", RuntimeContainerName)
	return ""
}

func autoUpdateSpec(enabled bool) *typeclawv1alpha1.AutoUpdateSpec {
	return &typeclawv1alpha1.AutoUpdateSpec{
		Enabled:                    enabled,
		Track:                      "latest",
		ConfirmationTimeoutMinutes: 15,
	}
}

func autoHoursAgo(hours float64) *metav1.Time {
	at := metav1.NewTime(time.Now().Add(-time.Duration(hours * float64(time.Hour))))
	return &at
}

// autoPromote seeds an in-flight rollout status for the given target.
func autoPromote(target string, phase typeclawv1alpha1.UpdatePhase, deadline *metav1.Time) *typeclawv1alpha1.UpdateStatus {
	return &typeclawv1alpha1.UpdateStatus{
		Phase:                phase,
		CurrentVersion:       "0.48.7",
		TargetVersion:        target,
		PromotedImage:        resources.DefaultRuntimeRepository + ":" + target,
		ConfirmationDeadline: deadline,
		Message:              "promoted",
	}
}

func autoGetInstance(t *testing.T, c client.Client, key types.NamespacedName) typeclawv1alpha1.TypeClawInstance {
	t.Helper()
	var got typeclawv1alpha1.TypeClawInstance
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("instance read-back: %v", err)
	}
	return got
}

func TestAutoUpdateDisabledResetsToIdle(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.AutoUpdate = autoUpdateSpec(false)
		in.Status.Update = autoPromote("0.49.0", update.PhaseUpdating,
			func() *metav1.Time { return autoHoursAgo(-1) }())
	})
	r, c := autoReconcilerFor(t, autoTagsFake(t, []string{"0.49.0"}), in)

	autoReconcile(t, r, autoKey(in))

	got := autoGetInstance(t, c, autoKey(in))
	u := got.Status.Update
	if u == nil || u.Phase != update.PhaseIdle {
		t.Fatalf("phase = %+v, want Idle", u)
	}
	if u.TargetVersion != "" || u.PromotedImage != "" || u.ConfirmationDeadline != nil || u.Message != "" {
		t.Errorf("rollout bookkeeping must be cleared on disable, got %+v", u)
	}
	if cond := condition(&got.Status, ConditionAutoUpdateReady); cond != nil {
		t.Errorf("AutoUpdateReady must be removed when disabled, got %+v", cond)
	}
}

func TestAutoUpdateGateBlocksOnStaleBackup(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime.Version = "0.48.7"
		in.Spec.AutoUpdate = autoUpdateSpec(true)
		in.Spec.AutoUpdate.RequireFreshBackup = true
		in.Spec.AutoUpdate.MaxBackupAgeHours = 24
		in.Status.Backup = &typeclawv1alpha1.BackupStatus{
			LatestSnapshot:   "kakao-agent-backup-1",
			LastSnapshotTime: autoHoursAgo(48),
		}
	})
	sts := autoWorkload(t, in, 0)
	r, c := autoReconcilerFor(t, autoTagsFake(t, []string{"0.48.7", "0.49.0"}), in, sts)

	autoReconcile(t, r, autoKey(in))

	got := autoGetInstance(t, c, autoKey(in))
	u := got.Status.Update
	if u == nil || u.Phase != update.PhaseAwaitingBackup {
		t.Fatalf("phase = %+v, want AwaitingBackup", u)
	}
	cond := condition(&got.Status, ConditionAutoUpdateReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonUpdAwaitingFreshBackup {
		t.Fatalf("AutoUpdateReady = %+v, want False/%s", cond, reasonUpdAwaitingFreshBackup)
	}
	if img := autoRuntimeImage(t, c, autoKey(in)); img != resources.ResolveRuntimeImage(in.Spec) {
		t.Errorf("gated rollout must not touch the workload image, got %q", img)
	}
}

func TestAutoUpdatePromotesNewCandidate(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime.Version = "0.48.7"
		in.Spec.AutoUpdate = autoUpdateSpec(true)
	})
	sts := autoWorkload(t, in, 0)
	r, c := autoReconcilerFor(t, autoTagsFake(t, []string{"0.47.1", "0.48.7", "0.49.0"}), in, sts)

	autoReconcile(t, r, autoKey(in))

	wantImage := resources.DefaultRuntimeRepository + ":0.49.0"
	if img := autoRuntimeImage(t, c, autoKey(in)); img != wantImage {
		t.Fatalf("runtime image = %q, want %q", img, wantImage)
	}
	got := autoGetInstance(t, c, autoKey(in))
	u := got.Status.Update
	if u == nil || u.Phase != update.PhaseUpdating || u.TargetVersion != "0.49.0" || u.PromotedImage != wantImage {
		t.Fatalf("status.update = %+v, want Updating toward 0.49.0", u)
	}
	cond := condition(&got.Status, ConditionAutoUpdateReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonUpdRolloutInProgress {
		t.Fatalf("AutoUpdateReady = %+v, want False/%s", cond, reasonUpdRolloutInProgress)
	}
}

func TestAutoUpdateIgnoresOlderCandidates(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime.Version = "0.48.7"
		in.Spec.AutoUpdate = autoUpdateSpec(true)
		// The confirmed version already matches the track's best release.
		in.Status.Update = &typeclawv1alpha1.UpdateStatus{Phase: update.PhaseReady, CurrentVersion: "0.49.0"}
	})
	sts := autoWorkload(t, in, 1)
	r, c := autoReconcilerFor(t, autoTagsFake(t, []string{"0.48.7", "0.49.0"}), in, sts)

	autoReconcile(t, r, autoKey(in))

	got := autoGetInstance(t, c, autoKey(in))
	if u := got.Status.Update; u != nil && u.Phase == update.PhaseUpdating {
		t.Fatalf("older candidate must not start a rollout, got %+v", u)
	}
	if img := autoRuntimeImage(t, c, autoKey(in)); img != resources.ResolveRuntimeImage(in.Spec) {
		t.Errorf("steady state must not touch the workload image, got %q", img)
	}
}

func TestAutoUpdateConfirmingOpensDeadlineOnce(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime.Version = "0.48.7"
		in.Spec.AutoUpdate = autoUpdateSpec(true)
		in.Status.Update = autoPromote("0.49.0", update.PhaseUpdating, nil)
	})
	sts := autoWorkload(t, in, 1)
	r, c := autoReconcilerFor(t, autoTagsFake(t, []string{"0.49.0"}), in, sts)
	key := autoKey(in)

	before := time.Now()
	autoReconcile(t, r, key)

	got := autoGetInstance(t, c, key)
	u := got.Status.Update
	if u.Phase != update.PhaseConfirming || u.ConfirmationDeadline == nil {
		t.Fatalf("phase/deadline = %+v, want Confirming with a deadline", u)
	}
	if u.ConfirmationDeadline.Time.Before(before.Add(14*time.Minute)) ||
		u.ConfirmationDeadline.Time.After(before.Add(16*time.Minute)) {
		t.Errorf("deadline %v not ~15m after reconcile start %v", u.ConfirmationDeadline.Time, before)
	}
	firstDeadline := *u.ConfirmationDeadline

	autoReconcile(t, r, key)
	got = autoGetInstance(t, c, key)
	if !got.Status.Update.ConfirmationDeadline.Time.Equal(firstDeadline.Time) {
		t.Errorf("deadline moved on repeat reconcile: %v → %v",
			firstDeadline.Time, got.Status.Update.ConfirmationDeadline.Time)
	}
	if got.Status.Update.Phase != update.PhaseConfirming {
		t.Errorf("phase = %q while inside window, want Confirming", got.Status.Update.Phase)
	}
}

func TestAutoUpdateConfirmsToReadyAfterWindow(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime.Version = "0.48.7"
		in.Spec.AutoUpdate = autoUpdateSpec(true)
		in.Status.Update = autoPromote("0.49.0", update.PhaseConfirming, autoHoursAgo(1))
	})
	sts := autoWorkload(t, in, 1)
	r, c := autoReconcilerFor(t, autoTagsFake(t, []string{"0.49.0"}), in, sts)

	autoReconcile(t, r, autoKey(in))

	got := autoGetInstance(t, c, autoKey(in))
	u := got.Status.Update
	if u.Phase != update.PhaseReady || u.CurrentVersion != "0.49.0" {
		t.Fatalf("phase/version = %+v, want Ready at 0.49.0", u)
	}
	if u.PromotedImage != "" || u.ConfirmationDeadline != nil || u.TargetVersion != "" {
		t.Errorf("confirmation must clear bookkeeping, got %+v", u)
	}
	cond := condition(&got.Status, ConditionAutoUpdateReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonUpdRolloutConfirmed {
		t.Fatalf("AutoUpdateReady = %+v, want True/%s", cond, reasonUpdRolloutConfirmed)
	}
}

func TestAutoUpdateRollsBackPastDeadlineWithoutReadiness(t *testing.T) {
	in := instance("kakao-agent", func(in *typeclawv1alpha1.TypeClawInstance) {
		in.Spec.Runtime.Version = "0.48.7"
		in.Spec.AutoUpdate = autoUpdateSpec(true)
		in.Status.Update = autoPromote("0.49.0", update.PhaseConfirming, autoHoursAgo(1))
	})
	sts := autoWorkload(t, in, 0)
	// The workload still carries the promoted image when the window expires.
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == RuntimeContainerName {
			sts.Spec.Template.Spec.Containers[i].Image = resources.DefaultRuntimeRepository + ":0.49.0"
		}
	}
	r, c := autoReconcilerFor(t, autoTagsFake(t, []string{"0.49.0"}), in, sts)

	autoReconcile(t, r, autoKey(in))

	fallback := resources.ResolveRuntimeImage(in.Spec)
	if img := autoRuntimeImage(t, c, autoKey(in)); img != fallback {
		t.Fatalf("runtime image = %q, want rollback to %q", img, fallback)
	}
	got := autoGetInstance(t, c, autoKey(in))
	u := got.Status.Update
	if u.Phase != update.PhaseRolledBack {
		t.Fatalf("phase = %+v, want RolledBack", u)
	}
	if !strings.Contains(u.Message, "rolled back to 0.48.7") {
		t.Errorf("message %q must name the rolled-back-to version", u.Message)
	}
	if u.PromotedImage != "" || u.ConfirmationDeadline != nil {
		t.Errorf("rollback must clear promoted image and deadline, got %+v", u)
	}
	if cond := condition(&got.Status, ConditionAutoUpdateReady); cond == nil || cond.Reason != reasonUpdRolledBack {
		t.Errorf("AutoUpdateReady = %+v, want reason %s", cond, reasonUpdRolledBack)
	}
}
