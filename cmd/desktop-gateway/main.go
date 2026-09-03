// Command /desktop-gateway is the Desktop Gateway baked into the operator
// image. One process fronts one Personal Desktop: it serves the agent API on
// the agent listener and the Desktop Console on the console listener, sharing
// a single input-control registry between them. All decision logic lives in
// internal/desktopgateway; this file only loads configuration, builds the
// KubeVirt access path, and wires signal shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/fml09/typeclaw-operator/internal/desktopgateway"
)

// exitConfiguration is returned for anything that makes the process
// unstartable, so a misconfigured Deployment crash-loops with a distinct code
// instead of looking like a runtime fault.
const exitConfiguration = 2

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := desktopgateway.LoadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitConfiguration)
	}
	restConfig, err := kubernetesConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Kubernetes client config: %v\n", err)
		os.Exit(exitConfiguration)
	}
	kubevirt, err := desktopgateway.NewKubeVirtClient(restConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build KubeVirt client: %v\n", err)
		os.Exit(exitConfiguration)
	}
	gateway, err := desktopgateway.New(cfg, kubevirt, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Desktop Gateway: %v\n", err)
		os.Exit(exitConfiguration)
	}
	agentListener, consoleListener, err := desktopgateway.Listen(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitConfiguration)
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	if err := gateway.Serve(ctx, agentListener, consoleListener); err != nil {
		logger.Error("desktop gateway stopped", "error", err)
		os.Exit(1)
	}
}

// kubernetesConfig prefers the Pod's ServiceAccount credential, which is how
// the gateway runs in production. A kubeconfig is only consulted outside the
// cluster, where dev console auth mode is the point.
func kubernetesConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, err
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
}
