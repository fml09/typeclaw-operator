// Package relay implements the restart relay binary: a polling consumer for
// the Managed Control Dir restart spool. The managed runtime writes atomic
// restart-*.json drops; the relay validates each drop against its own runtime
// identity, deletes its own Pod (the at-least-once actuation that forces the
// StatefulSet to recreate the runtime), then archives the drop under
// consumed/ as the dedupe marker. Cluster access is injected through the
// PodDeleter seam so every decision path stays unit-testable without an API
// server.
package relay

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// RestartRequest mirrors the restart spool drop schema, version 1.
type RestartRequest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Kind          string `json:"kind"`
	RequestID     string `json:"requestId"`
	RuntimeID     string `json:"runtimeId"`
	RequestedAt   string `json:"requestedAt"`
}

// PodDeleter is the cluster seam of the watcher.
type PodDeleter interface {
	DeletePod(ctx context.Context, name, namespace string) error
}

const (
	dropGlob       = "restart-*.json"
	consumedDir    = "consumed"
	schemaVersion1 = 1
	kindRestart    = "restart"

	// DefaultInterval is the polling cadence when Watcher.Interval is unset.
	DefaultInterval = 500 * time.Millisecond
)

// Watcher polls ControlDir and translates valid restart drops into Pod
// deletions. All fields are required except Interval (defaults to
// DefaultInterval) and Log (defaults to slog.Default).
type Watcher struct {
	ControlDir string
	RuntimeID  string
	PodName    string
	Namespace  string
	Interval   time.Duration
	Deleter    PodDeleter
	Log        *slog.Logger
}

// Run polls until ctx is cancelled. It always returns ctx.Err() on shutdown;
// individual drop failures are logged and retried on the next tick rather
// than aborting the loop.
func (w *Watcher) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	log := w.logger()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("restart relay shutting down")
			return ctx.Err()
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

func (w *Watcher) pollOnce(ctx context.Context) {
	matches, err := filepath.Glob(filepath.Join(w.ControlDir, dropGlob))
	if err != nil {
		w.logger().Error("scanning control dir", "err", err)
		return
	}
	sort.Strings(matches)
	for _, path := range matches {
		w.consume(ctx, path)
	}
}

// consume handles one drop exactly once per file: validate, actuate, archive.
// Any validation failure removes the file (warn + continue) so a poisoned
// drop cannot wedge the loop; an actuation failure keeps the file so the next
// tick retries (at-least-once).
func (w *Watcher) consume(ctx context.Context, path string) {
	log := w.logger()

	data, err := os.ReadFile(path)
	if err != nil {
		log.Warn("unreadable restart drop, removing", "path", path, "err", err)
		w.drop(path)
		return
	}

	var req RestartRequest
	if err := json.Unmarshal(data, &req); err != nil {
		log.Warn("malformed restart drop, removing", "path", path, "err", err)
		w.drop(path)
		return
	}
	switch {
	case req.SchemaVersion != schemaVersion1:
		log.Warn("unsupported schemaVersion in restart drop, removing",
			"path", path, "schemaVersion", req.SchemaVersion)
		w.drop(path)
		return
	case req.Kind != kindRestart:
		log.Warn("unexpected kind in restart drop, removing",
			"path", path, "kind", req.Kind)
		w.drop(path)
		return
	case req.RequestID == "" || strings.ContainsAny(req.RequestID, `/\`):
		log.Warn("invalid requestId in restart drop, removing", "path", path)
		w.drop(path)
		return
	case req.RuntimeID != w.RuntimeID:
		log.Warn("restart drop for foreign runtime identity, removing",
			"path", path, "dropRuntimeId", req.RuntimeID, "ownRuntimeId", w.RuntimeID)
		w.drop(path)
		return
	}

	marker := w.consumedPath(req.RequestID)
	if _, err := os.Stat(marker); err == nil {
		log.Info("duplicate restart drop already consumed, skipping",
			"path", path, "requestId", req.RequestID)
		w.drop(path)
		return
	}

	if err := w.Deleter.DeletePod(ctx, w.PodName, w.Namespace); err != nil {
		// A repeat delete of a gone Pod is success: the previous pass (or the
		// controller) already removed it.
		if !apierrors.IsNotFound(err) {
			log.Error("pod deletion failed, will retry drop",
				"path", path, "requestId", req.RequestID, "err", err)
			return
		}
		log.Info("pod already gone, treating delete as done",
			"requestId", req.RequestID)
	}

	if err := w.archive(path, marker); err != nil {
		log.Error("archiving consumed drop failed, will retry",
			"path", path, "marker", marker, "err", err)
		return
	}
	log.Info("restart request satisfied",
		"requestId", req.RequestID, "runtimeId", req.RuntimeID,
		"requestedAt", req.RequestedAt)
}

// archive moves the drop into consumed/ where its presence doubles as the
// dedupe marker.
func (w *Watcher) archive(src, marker string) error {
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return err
	}
	return os.Rename(src, marker)
}

// drop removes an incoming file that must not be acted upon.
func (w *Watcher) drop(path string) {
	if err := os.Remove(path); err != nil {
		w.logger().Warn("removing rejected drop failed", "path", path, "err", err)
	}
}

func (w *Watcher) consumedPath(requestID string) string {
	return filepath.Join(w.ControlDir, consumedDir, "restart-"+requestID+".json")
}

func (w *Watcher) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
