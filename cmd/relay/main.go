// Command /relay is the restart relay sidecar baked into the operator image.
// It runs inside every TypeClaw Instance Pod, polls the Managed Control Dir
// for restart-request drops (see internal/relay), and deletes its own Pod to
// force runtime replacement. The binary stays thin: all decision logic lives
// in internal/relay behind the PodDeleter seam; this file only wires
// environment config, the projected-token REST client, and signal shutdown.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/relay"
	"github.com/fml09/typeclaw-operator/internal/resources"
)

const (
	// apiHost is the in-cluster Kubernetes API service endpoint.
	apiHost = "https://kubernetes.default.svc"

	// saCAFile is the service-account CA bundle every Pod receives.
	saCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// missingEnvError reports required sidecar environment that the Pod spec
// failed to project.
type missingEnvError struct{ env []string }

func (e *missingEnvError) Error() string {
	return "required environment not set: " + strings.Join(e.env, ", ")
}

// clientPodDeleter adapts the controller-runtime client to relay.PodDeleter.
type clientPodDeleter struct {
	c client.Client
}

func (d clientPodDeleter) DeletePod(ctx context.Context, name, namespace string) error {
	return d.c.Delete(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// clientConfigObserver persists config observations into the Instance's
// selfConfig status block via a merge patch scoped to that field only, and
// mirrors them into the watcher's snapshot so revision counting advances.
type clientConfigObserver struct{ c client.Client }

func (o clientConfigObserver) Observe(
	ctx context.Context,
	in *typeclawv1alpha1.TypeClawInstance,
	obs relay.ConfigObservation,
) error {
	base := in.DeepCopy()
	in.Status.SelfConfig = &typeclawv1alpha1.SelfConfigStatus{
		ObservedDigest:     obs.Digest,
		ObservedAt:         &metav1.Time{Time: obs.At},
		Revision:           obs.Revision,
		ChangedPaths:       obs.ChangedPaths,
		ProtectedViolation: obs.ProtectedViolation,
	}
	return o.c.Status().Patch(ctx, in, client.MergeFrom(base))
}

func intervalFromEnv(raw string) (time.Duration, error) {
	if raw == "" {
		return relay.DefaultInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, errors.New("TYPECLAW_RELAY_INTERVAL must be a positive duration, got " + raw)
	}
	return d, nil
}

func run(ctx context.Context, log *slog.Logger) error {
	controlDir := envOr("TYPECLAW_MANAGED_CONTROL_DIR", resources.ManagedControlDir)
	runtimeID := os.Getenv("TYPECLAW_RUNTIME_ID")
	podName := os.Getenv("POD_NAME")
	namespace := os.Getenv("POD_NAMESPACE")
	var missing []string
	for _, kv := range []struct{ name, value string }{
		{"TYPECLAW_RUNTIME_ID", runtimeID},
		{"POD_NAME", podName},
		{"POD_NAMESPACE", namespace},
	} {
		if strings.TrimSpace(kv.value) == "" {
			missing = append(missing, kv.name)
		}
	}
	if len(missing) > 0 {
		return &missingEnvError{env: missing}
	}

	interval, err := intervalFromEnv(os.Getenv("TYPECLAW_RELAY_INTERVAL"))
	if err != nil {
		return err
	}

	// rest.InClusterConfig would demand the fixed service-account token path,
	// but the relay identity arrives via the projected relay token volume —
	// so the config is constructed explicitly around that file.
	cfg := &rest.Config{
		Host:            apiHost,
		BearerTokenFile: filepath.Join(resources.RelayTokenMountPath, resources.RelayTokenFileName),
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: false,
			CAFile:   saCAFile,
		},
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	instance := &typeclawv1alpha1.TypeClawInstance{
		ObjectMeta: metav1.ObjectMeta{Name: strings.TrimSuffix(podName, "-0"), Namespace: namespace},
	}
	// Fetch the live spec so the watcher evaluates protected paths against
	// the current policy, not a stale env snapshot.
	if err := c.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
		return err
	}

	errCh := make(chan error, 2)
	go func() {
		w := &relay.Watcher{
			ControlDir: controlDir,
			RuntimeID:  runtimeID,
			PodName:    podName,
			Namespace:  namespace,
			Interval:   interval,
			Deleter:    clientPodDeleter{c},
			Log:        log,
		}
		log.Info("restart relay watching",
			"controlDir", controlDir, "runtimeId", runtimeID,
			"pod", namespace+"/"+podName, "interval", interval)
		errCh <- w.Run(ctx)
	}()

	if instance.Spec.SelfConfig != nil {
		go func() {
			cw := &relay.ConfigWatcher{
				Instance: instance,
				AgentDir: resources.AgentMountPath,
				Interval: interval,
				Observer: clientConfigObserver{c},
				Log:      log,
			}
			log.Info("selfconfig observation watching", "agentDir", resources.AgentMountPath)
			errCh <- cw.Run(ctx)
		}()
	}

	return <-errCh
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("restart relay failed", "err", err)
		os.Exit(1)
	}
}
