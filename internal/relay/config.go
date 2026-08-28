package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"syscall"
	"time"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
)

// ConfigObservation is one sanitized sighting of a TypeClaw configuration,
// ready to be persisted into status.selfConfig. It contains no configuration
// values; only a full-document digest and per-top-level-key digests cross the
// runtime-to-relay boundary.
type ConfigObservation struct {
	Digest             string
	At                 time.Time
	Revision           int64
	ChangedPaths       []string
	ProtectedViolation bool
}

// ConfigObservationDocument is the file contract emitted by the Managed
// Runtime. Values are digests, never the raw typeclaw.json values.
type ConfigObservationDocument struct {
	Digest string            `json:"digest"`
	Values map[string]string `json:"values"`
}

// ConfigObserver is the cluster seam of the config watcher. Implementations
// must persist the observation AND reflect it into in.Status.SelfConfig
// (digest, at, revision, changed paths, violation), because the watcher
// derives the next revision from that state.
type ConfigObserver interface {
	Observe(ctx context.Context, in *typeclawv1alpha1.TypeClawInstance, obs ConfigObservation) error
}

// ConfigWatcher polls a sanitized runtime-to-relay observation file and turns
// content changes into ConfigObservations (ADR 0005). It never mounts or
// reads the Agent Folder, never reads secrets, and never writes the filesystem.
// The first sighting seeds the baseline: an observation with revision 0 and no
// changed paths, so operators can always see what is live right now and the
// first observation never trips policy.
type ConfigWatcher struct {
	Instance        *typeclawv1alpha1.TypeClawInstance
	ObservationFile string
	Interval        time.Duration
	Observer        ConfigObserver
	Log             *slog.Logger

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
	raw, err := readObservation(w.ObservationFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Managed Runtime has not emitted an observation yet.
		}
		return err
	}
	document, err := decodeObservationDocument(raw)
	if err != nil {
		return err
	}
	if document.Digest == w.lastDigest {
		return nil
	}

	obs := ConfigObservation{
		Digest: document.Digest,
		At:     w.nowFunc().UTC(),
	}
	if w.lastDigest == "" {
		// Baseline sighting: record what is live without calling it a change.
		obs.Revision = 0
	} else {
		obs.Revision = w.currentRevision() + 1
		obs.ChangedPaths = changedKeys(w.lastValues, document.Values)
		obs.ProtectedViolation = intersects(w.protectedPaths(), obs.ChangedPaths)
	}

	if err := w.Observer.Observe(ctx, w.Instance, obs); err != nil {
		return err // Digest stays uncommitted; the next tick retries.
	}
	w.lastDigest = document.Digest
	w.lastValues = document.Values
	return nil
}

const maxObservationBytes = 1 << 20

func readObservation(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxObservationBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxObservationBytes {
		return nil, errors.New("observation document exceeds size limit")
	}
	return raw, nil
}

func decodeObservationDocument(raw []byte) (ConfigObservationDocument, error) {
	var document ConfigObservationDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return ConfigObservationDocument{}, fmt.Errorf("decode observation: %w", err)
	}
	if document.Digest == "" {
		return ConfigObservationDocument{}, errors.New("observation digest is required")
	}
	if len(document.Digest) != sha256.Size*2 {
		return ConfigObservationDocument{}, errors.New("observation digest must be sha256 hex")
	}
	if _, err := hex.DecodeString(document.Digest); err != nil {
		return ConfigObservationDocument{}, errors.New("observation digest must be sha256 hex")
	}
	if document.Values == nil {
		document.Values = map[string]string{}
	}
	for key, digest := range document.Values {
		if key == "" {
			return ConfigObservationDocument{}, errors.New("observation key must not be empty")
		}
		if len(digest) != 16 {
			return ConfigObservationDocument{}, errors.New("observation key digest must be short sha256 hex")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return ConfigObservationDocument{}, errors.New("observation key digest must be short sha256 hex")
		}
	}
	return document, nil
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
