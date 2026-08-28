package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/fml09/typeclaw-operator/internal/credential"
)

// CredentialBrokerServer hosts the typed capability gateway. It is a
// controller-plane runnable: only the manager's Kubernetes identity creates
// CredentialRequest objects, while callers authenticate with SPIFFE mTLS.
type CredentialBrokerServer struct {
	Handler     *credential.Broker
	Address     string
	Certificate string
	PrivateKey  string
	ClientCA    string
	Server      *http.Server
}

func NewCredentialBrokerServer(handler *credential.Broker, address, certificate, privateKey, clientCA string) *CredentialBrokerServer {
	return &CredentialBrokerServer{
		Handler:     handler,
		Address:     address,
		Certificate: certificate,
		PrivateKey:  privateKey,
		ClientCA:    clientCA,
	}
}

// NeedLeaderElection is false so every Service endpoint can serve mTLS
// requests; request identity and deterministic names make writes idempotent.
func (s *CredentialBrokerServer) NeedLeaderElection() bool { return false }

func (s *CredentialBrokerServer) Start(ctx context.Context) error {
	if s.Handler == nil || s.Address == "" || s.Certificate == "" || s.PrivateKey == "" || s.ClientCA == "" {
		return errors.New("credential broker requires address, server certificate, private key, and client CA")
	}
	certificate, err := tls.LoadX509KeyPair(s.Certificate, s.PrivateKey)
	if err != nil {
		return err
	}
	caBytes, err := os.ReadFile(s.ClientCA)
	if err != nil {
		return err
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caBytes) {
		return errors.New("credential broker client CA contains no certificate")
	}
	config := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
	listener, err := net.Listen("tcp", s.Address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           s.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		TLSConfig:         config,
	}
	s.Server = server
	tlsListener := tls.NewListener(listener, config)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(tlsListener)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
