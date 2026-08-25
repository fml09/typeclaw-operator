package relay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

type recordingObserver struct {
	observations []ConfigObservation
	failFirst    int
}

func (r *recordingObserver) Observe(ctx context.Context, in *typeclawv1alpha1.TypeClawInstance, obs ConfigObservation) error {
	if r.failFirst > 0 {
		r.failFirst--
		return context.DeadlineExceeded
	}
	in.Status.SelfConfig = &typeclawv1alpha1.SelfConfigStatus{
		ObservedDigest:     obs.Digest,
		Revision:           obs.Revision,
		ChangedPaths:       obs.ChangedPaths,
		ProtectedViolation: obs.ProtectedViolation,
	}
	r.observations = append(r.observations, obs)
	return nil
}

func configWatcher(t *testing.T, dir string, policy []string) (*ConfigWatcher, *recordingObserver, *typeclawv1alpha1.TypeClawInstance) {
	t.Helper()
	in := &typeclawv1alpha1.TypeClawInstance{}
	in.Name = "kakao-agent"
	in.Namespace = "agents"
	in.Spec.SelfConfig = &typeclawv1alpha1.SelfConfigSpec{ProtectedPaths: policy}
	obs := &recordingObserver{}
	w := &ConfigWatcher{
		Instance: in,
		AgentDir: dir,
		Observer: obs,
		now:      func() time.Time { return time.Unix(1700000000, 0) },
	}
	return w, obs, in
}

func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "typeclaw.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestConfigWatcherBaselineSeedsWithoutChangedPaths(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"channels":{},"sandbox":{}}`)
	w, obs, _ := configWatcher(t, dir, nil)

	if err := w.pollOnce(context.Background()); err != nil {
		t.Fatalf("baseline poll: %v", err)
	}
	if len(obs.observations) != 1 {
		t.Fatalf("expected one baseline observation, got %d", len(obs.observations))
	}
	first := obs.observations[0]
	if first.Revision != 0 || first.ChangedPaths != nil || first.ProtectedViolation {
		t.Fatalf("baseline must be revision 0 with no changes: %+v", first)
	}
	if len(first.Digest) != 64 {
		t.Fatalf("digest must be sha256 hex, got %q", first.Digest)
	}
}

func TestConfigWatcherCountsChangesAndEvaluatesPolicy(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"channels":{},"sandbox":{}}`)
	w, obs, in := configWatcher(t, dir, []string{"sandbox"})
	ctx := context.Background()

	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// Adding a non-protected key increments the revision without violation.
	writeConfig(t, dir, `{"channels":{},"sandbox":{},"plugins":[]}`)
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("change poll: %v", err)
	}
	second := obs.observations[1]
	if second.Revision != 1 {
		t.Fatalf("revision = %d, want 1", second.Revision)
	}
	if got := second.ChangedPaths; len(got) != 1 || got[0] != "plugins" {
		t.Fatalf("changed paths = %v, want [plugins]", got)
	}
	if second.ProtectedViolation {
		t.Fatalf("plugins change must not violate sandbox policy")
	}

	// Changing a protected key flips the violation flag.
	writeConfig(t, dir, `{"channels":{},"sandbox":{"realProc":false},"plugins":[]}`)
	if err := w.pollOnce(ctx); err != nil {
		t.Fatalf("protected poll: %v", err)
	}
	third := obs.observations[2]
	if third.Revision != 2 {
		t.Fatalf("revision = %d, want 2 (persisted state drives counting)", third.Revision)
	}
	if !third.ProtectedViolation {
		t.Fatalf("sandbox change must violate protected policy")
	}
	_ = in
}

func TestConfigWatcherUnchangedContentEmitsNothing(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"a":1}`)
	w, obs, _ := configWatcher(t, dir, nil)
	for i := 0; i < 3; i++ {
		if err := w.pollOnce(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if len(obs.observations) != 1 {
		t.Fatalf("identical content must dedupe to the baseline only, got %d", len(obs.observations))
	}
}

func TestConfigWatcherMissingFileIsQuietlyIgnored(t *testing.T) {
	w, obs, _ := configWatcher(t, t.TempDir(), nil)
	if err := w.pollOnce(context.Background()); err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if len(obs.observations) != 0 {
		t.Fatalf("no file means no observation, got %d", len(obs.observations))
	}
}

func TestConfigWatcherRetriesFailedObservationWithoutLosingState(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"a":1}`)
	w, obs, _ := configWatcher(t, dir, nil)
	obs.failFirst = 1

	if err := w.pollOnce(context.Background()); err == nil {
		t.Fatalf("expected observer failure to surface")
	}
	// The digest is not committed on failure, so a retry observes once.
	if err := w.pollOnce(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(obs.observations) != 1 {
		t.Fatalf("failed observation must not double-count, got %d", len(obs.observations))
	}
}
func TestValueEditsUnderExistingKeysAreDetected(t *testing.T) {
	prev := topLevelValueDigests([]byte(`{"keep":1,"drop":2}`))
	next := topLevelValueDigests([]byte(`{"keep":2,"add":3}`))
	got := changedKeys(prev, next)
	if len(got) != 3 {
		t.Fatalf("changed keys = %v, want [add drop keep]", got)
	}
}
