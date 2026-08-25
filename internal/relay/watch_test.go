package relay

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeDeleter records actuations and can simulate API failures.
type fakeDeleter struct {
	calls []string
	err   error
}

func (f *fakeDeleter) DeletePod(_ context.Context, name, namespace string) error {
	f.calls = append(f.calls, namespace+"/"+name)
	return f.err
}

type watcherOpt func(*Watcher, *fakeDeleter)

func newTestWatcher(t *testing.T, opts ...watcherOpt) (*Watcher, *fakeDeleter, string) {
	t.Helper()
	dir := t.TempDir()
	d := &fakeDeleter{}
	w := &Watcher{
		ControlDir: dir,
		RuntimeID:  "agents/kakao-agent",
		PodName:    "kakao-agent-0",
		Namespace:  "agents",
		Deleter:    d,
		Log:        testLogger(t),
	}
	for _, opt := range opts {
		opt(w, d)
	}
	return w, d, dir
}

func dropFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing drop: %v", err)
	}
}

func validDrop(requestID string) string {
	return `{"schemaVersion":1,"kind":"restart","requestId":"` + requestID +
		`","runtimeId":"agents/kakao-agent","requestedAt":"2026-08-25T00:00:00Z"}`
}

func poll(t *testing.T, w *Watcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w.pollOnce(ctx)
}

func TestValidRequestDeletesPodThenArchives(t *testing.T) {
	w, d, dir := newTestWatcher(t)
	dropFile(t, dir, "restart-a1.json", validDrop("a1"))

	poll(t, w)

	want := "agents/kakao-agent-0"
	if len(d.calls) != 1 || d.calls[0] != want {
		t.Fatalf("DeletePod calls = %v, want [%s]", d.calls, want)
	}
	marker := filepath.Join(dir, "consumed", "restart-a1.json")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("consumed marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "restart-a1.json")); !os.IsNotExist(err) {
		t.Fatalf("incoming drop still present: %v", err)
	}
}

func TestForeignRuntimeIDDroppedWithoutActuation(t *testing.T) {
	w, d, dir := newTestWatcher(t)
	dropFile(t, dir, "restart-f1.json",
		`{"schemaVersion":1,"kind":"restart","requestId":"f1","runtimeId":"agents/other-agent","requestedAt":"2026-08-25T00:00:00Z"}`)

	poll(t, w)

	if len(d.calls) != 0 {
		t.Fatalf("DeletePod called for foreign runtimeId: %v", d.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "restart-f1.json")); !os.IsNotExist(err) {
		t.Fatalf("foreign drop not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "consumed")); !os.IsNotExist(err) {
		t.Fatalf("consumed dir created for foreign drop: %v", err)
	}
}

func TestMalformedDropsRemovedWithoutActuation(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"not json", `{oops`},
		{"wrong kind", `{"schemaVersion":1,"kind":"reap","requestId":"m2","runtimeId":"agents/kakao-agent","requestedAt":"x"}`},
		{"wrong schema version", `{"schemaVersion":2,"kind":"restart","requestId":"m3","runtimeId":"agents/kakao-agent","requestedAt":"x"}`},
		{"empty request id", `{"schemaVersion":1,"kind":"restart","requestId":"","runtimeId":"agents/kakao-agent","requestedAt":"x"}`},
		{"path traversal request id", `{"schemaVersion":1,"kind":"restart","requestId":"../escape","runtimeId":"agents/kakao-agent","requestedAt":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, d, dir := newTestWatcher(t)
			name := "restart-" + tc.name + ".json"
			dropFile(t, dir, name, tc.content)

			poll(t, w)

			if len(d.calls) != 0 {
				t.Fatalf("DeletePod called for malformed drop: %v", d.calls)
			}
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("malformed drop not removed: %v", err)
			}
		})
	}
}

func TestConsumedMarkerSkipsDuplicate(t *testing.T) {
	w, d, dir := newTestWatcher(t)
	if err := os.MkdirAll(filepath.Join(dir, "consumed"), 0o755); err != nil {
		t.Fatalf("mkdir consumed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "consumed", "restart-dup.json"), nil, 0o644); err != nil {
		t.Fatalf("seeding marker: %v", err)
	}
	dropFile(t, dir, "restart-dup.json", validDrop("dup"))

	poll(t, w)

	if len(d.calls) != 0 {
		t.Fatalf("DeletePod called for already-consumed drop: %v", d.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "restart-dup.json")); !os.IsNotExist(err) {
		t.Fatalf("duplicate incoming drop not removed: %v", err)
	}
}

func TestGonePodDeleteToleratedAndArchived(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, "kakao-agent-0")
	w, d, dir := newTestWatcher(t, func(w *Watcher, d *fakeDeleter) {
		d.err = notFound
	})
	dropFile(t, dir, "restart-gone.json", validDrop("gone"))

	poll(t, w)

	if len(d.calls) != 1 {
		t.Fatalf("DeletePod calls = %v, want exactly one", d.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "consumed", "restart-gone.json")); err != nil {
		t.Fatalf("gone-pod drop not archived: %v", err)
	}
}

func TestDoubleInvocationActuatesOnce(t *testing.T) {
	w, d, dir := newTestWatcher(t)
	dropFile(t, dir, "restart-twice.json", validDrop("twice"))

	poll(t, w)
	poll(t, w)

	if len(d.calls) != 1 {
		t.Fatalf("DeletePod calls after two passes = %v, want one", d.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "consumed", "restart-twice.json")); err != nil {
		t.Fatalf("marker missing after first pass: %v", err)
	}
}

func TestFailedDeletionRetainsDropForRetry(t *testing.T) {
	w, _, dir := newTestWatcher(t, func(w *Watcher, d *fakeDeleter) {
		d.err = apierrors.NewServiceUnavailable("apiserver down")
	})
	dropFile(t, dir, "retry-me.json", validDrop("retry"))

	poll(t, w)

	if _, err := os.Stat(filepath.Join(dir, "retry-me.json")); err != nil {
		t.Fatalf("drop must be retained for retry on real failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "consumed", "restart-retry.json")); !os.IsNotExist(err) {
		t.Fatal("failed drop must not be archived")
	}
}
