package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"time"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// ConfigObservation is one sighting of typeclaw.json state, ready to be
// persisted into status.selfConfig.
type ConfigObservation struct {
	Digest             string
	At                 time.Time
	Revision           int64
	ChangedPaths       []string
	ProtectedViolation bool
}

// ConfigObserver is the cluster seam of the config watcher. Implementations
// must persist the observation AND reflect it into in.Status.SelfConfig
// (digest, at, revision, changed paths, violation), because the watcher
// derives the next revision from that state.
type ConfigObserver interface {
	Observe(ctx context.Context, in *typeclawv1alpha1.TypeClawInstance, obs ConfigObservation) error
}

// ConfigWatcher polls <agentDir>/typeclaw.json and turns content changes
// into ConfigObservations (ADR 0005). It never reads secrets files and never
// writes the filesystem. The first sighting seeds the baseline: an
// observation with revision 0 and no changed paths, so operators can always
// see what is live right now and the first observation never trips policy.
type ConfigWatcher struct {
	Instance *typeclawv1alpha1.TypeClawInstance
	AgentDir string
	Interval time.Duration
	Observer ConfigObserver
	Log      *slog.Logger

	now        func() time.Time
	lastDigest string
	lastValues map[string]string
}

func (w *ConfigWatcher) nowFunc() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *ConfigWatcher) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// Run polls until ctx is cancelled; per-tick failures are logged and retried.
func (w *ConfigWatcher) Run(ctx context.Context) error {
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
			log.Info("config watcher shutting down")
			return ctx.Err()
		case <-ticker.C:
			if err := w.pollOnce(ctx); err != nil {
				log.Warn("config observation failed", "error", err)
			}
		}
	}
}

func (w *ConfigWatcher) pollOnce(ctx context.Context) error {
	raw, err := os.ReadFile(w.AgentDir + "/typeclaw.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Agent Folder not initialized yet.
		}
		return err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if digest == w.lastDigest {
		return nil
	}

	values := topLevelValueDigests(raw)

	obs := ConfigObservation{
		Digest: digest,
		At:     w.nowFunc().UTC(),
	}
	if w.lastDigest == "" {
		// Baseline sighting: record what is live without calling it a change.
		obs.Revision = 0
	} else {
		obs.Revision = w.currentRevision() + 1
		obs.ChangedPaths = changedKeys(w.lastValues, values)
		obs.ProtectedViolation = intersects(w.protectedPaths(), obs.ChangedPaths)
	}

	if err := w.Observer.Observe(ctx, w.Instance, obs); err != nil {
		return err // Digest stays uncommitted; the next tick retries.
	}
	w.lastDigest = digest
	w.lastValues = values
	return nil
}

// currentRevision reads the persisted revision the Observer wrote back into
// the Instance snapshot after the last successful observation.
func (w *ConfigWatcher) currentRevision() int64 {
	if u := w.Instance.Status.SelfConfig; u != nil {
		return u.Revision
	}
	return 0
}

func (w *ConfigWatcher) protectedPaths() []string {
	if s := w.Instance.Spec.SelfConfig; s != nil {
		return s.ProtectedPaths
	}
	return nil
}

// topLevelValueDigests maps each top-level object key to a short SHA-256 of
// its raw JSON value, so value edits under an existing key are detected
// exactly like additions and removals.
func topLevelValueDigests(raw []byte) map[string]string {
	var doc map[string]json.RawMessage
	out := map[string]string{}
	if json.Unmarshal(raw, &doc) == nil {
		for k, v := range doc {
			sum := sha256.Sum256(v)
			out[k] = hex.EncodeToString(sum[:8])
		}
	}
	return out
}

func changedKeys(prev, next map[string]string) []string {
	var out []string
	for k, v := range next {
		if pv, ok := prev[k]; !ok || pv != v {
			out = append(out, k)
		}
	}
	for k := range prev {
		if _, ok := next[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func intersects(policy []string, changed []string) bool {
	set := make(map[string]struct{}, len(policy))
	for _, p := range policy {
		set[p] = struct{}{}
	}
	for _, c := range changed {
		if _, ok := set[c]; ok {
			return true
		}
	}
	return false
}
