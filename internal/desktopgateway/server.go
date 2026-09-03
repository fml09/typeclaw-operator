package desktopgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	// revokeTimeout bounds how long shutdown waits for live controllers to
	// observe that their lease is gone.
	revokeTimeout = 3 * time.Second
	// shutdownTimeout bounds the graceful close of both listeners.
	shutdownTimeout = 5 * time.Second
)

// Listen binds both listeners. They are bound before Serve so a port conflict
// fails at startup rather than after the process reports itself healthy.
func Listen(cfg Config) (agent net.Listener, console net.Listener, err error) {
	agent, err = net.Listen("tcp", cfg.AgentListenAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("bind agent listener: %w", err)
	}
	console, err = net.Listen("tcp", cfg.ConsoleListenAddress)
	if err != nil {
		_ = agent.Close()
		return nil, nil, fmt.Errorf("bind console listener: %w", err)
	}
	return agent, console, nil
}

// Serve runs the agent API and the Desktop Console on their own servers until
// ctx is cancelled or either listener fails. Both listeners share this
// Gateway, so an input lease taken through one is visible on the other.
//
// In dev console auth mode the console listener must have bound a loopback
// address (ticket #19); Serve refuses to start otherwise, because the mode
// authenticates from a query parameter and has no other locality evidence.
func (g *Gateway) Serve(ctx context.Context, agentListener, consoleListener net.Listener) error {
	if g.config.ConsoleAuthMode == ConsoleAuthDev {
		if err := RequireLoopbackConsoleListener(consoleListener.Addr()); err != nil {
			return err
		}
		g.devLoopbackListener = true
	}

	agentServer := &http.Server{
		Handler:           g.AgentHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	consoleServer := &http.Server{
		Handler:           g.ConsoleHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	g.logger.Info("starting desktop gateway",
		"desktop", g.config.Name,
		"namespace", g.config.Namespace,
		"os", g.config.OS,
		"agentAddress", agentListener.Addr().String(),
		"consoleAddress", consoleListener.Addr().String(),
		"consoleAuthMode", string(g.config.ConsoleAuthMode),
		"bootID", g.bootID,
	)

	serverErrors := make(chan error, 2)
	go func() { serverErrors <- agentServer.Serve(agentListener) }()
	go func() { serverErrors <- consoleServer.Serve(consoleListener) }()

	var runErr error
	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-ctx.Done():
	}

	// Revoke first: a controller must learn it lost input before the socket
	// that carries it disappears underneath it.
	g.logger.Info("revoking desktop control before shutdown")
	revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), revokeTimeout)
	g.controls.revokeAll(revokeCtx)
	cancelRevoke()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := agentServer.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shut down agent listener: %w", err)
	}
	if err := consoleServer.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shut down console listener: %w", err)
	}
	return runErr
}
