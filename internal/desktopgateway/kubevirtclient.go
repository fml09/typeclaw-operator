package desktopgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

const (
	// subresourceAPIPath is the KubeVirt subresource API group the VNC,
	// screenshot, start, and stop endpoints live under.
	subresourceAPIPath = "/apis/subresources.kubevirt.io/v1"
	// vncSubprotocol is the KubeVirt plain RFB stream subprotocol.
	vncSubprotocol = "plain.kubevirt.io"
	// websocketBufferSize matches KubeVirt's own stream buffer so a single
	// RFB update rarely spans more frames than it has to.
	websocketBufferSize = 10 * 1024
	// vncHandshakeTimeout bounds the websocket upgrade itself; the caller's
	// context still bounds the whole dial.
	vncHandshakeTimeout = 10 * time.Second
)

// clusterKubeVirtClient reaches KubeVirt with client-go alone: the dynamic
// client for VM/VMI reads, a REST client for the subresource verbs, and a
// websocket dial for VNC. The KubeVirt Go client is deliberately not a
// dependency of the operator module.
type clusterKubeVirtClient struct {
	config  *rest.Config
	dynamic dynamic.Interface
	rest    rest.Interface
}

// NewKubeVirtClient builds the production KubeVirt access path from an
// in-cluster (or kubeconfig) REST config.
func NewKubeVirtClient(config *rest.Config) (KubeVirtClient, error) {
	if config == nil {
		return nil, errors.New("KubeVirt REST config is required")
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build KubeVirt dynamic client: %w", err)
	}
	subresourceConfig := rest.CopyConfig(config)
	subresourceConfig.APIPath = "/apis"
	subresourceConfig.GroupVersion = &metav1.SchemeGroupVersion
	// The subresource verbs answer with PNG bytes or an empty body, never a
	// typed object; the codec is only ever used to decode API error status.
	subresourceConfig.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	restClient, err := rest.UnversionedRESTClientFor(subresourceConfig)
	if err != nil {
		return nil, fmt.Errorf("build KubeVirt subresource client: %w", err)
	}
	return &clusterKubeVirtClient{config: config, dynamic: dynamicClient, rest: restClient}, nil
}

func (c *clusterKubeVirtClient) GetVM(ctx context.Context, namespace, name string) (*VirtualMachine, error) {
	object, err := c.dynamic.Resource(virtualMachineGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return decodeVirtualMachine(object)
}

func (c *clusterKubeVirtClient) GetVMI(ctx context.Context, namespace, name string) (*VirtualMachineInstance, error) {
	object, err := c.dynamic.Resource(virtualMachineInstanceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return decodeVirtualMachineInstance(object)
}

func (c *clusterKubeVirtClient) Start(ctx context.Context, namespace, name string) error {
	return c.rest.Verb(http.MethodPut).AbsPath(vmSubresourcePath(namespace, name, "start")).Do(ctx).Error()
}

func (c *clusterKubeVirtClient) Stop(ctx context.Context, namespace, name string) error {
	return c.rest.Verb(http.MethodPut).AbsPath(vmSubresourcePath(namespace, name, "stop")).Do(ctx).Error()
}

func (c *clusterKubeVirtClient) Screenshot(ctx context.Context, namespace, name string) ([]byte, error) {
	body, err := c.rest.Verb(http.MethodGet).
		AbsPath(vmiSubresourcePath(namespace, name, "vnc", "screenshot")).
		Param("moveCursor", "false").
		Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	// Read one byte past the processing limit so an oversized framebuffer is
	// still recognisable as oversized instead of silently truncated.
	return io.ReadAll(io.LimitReader(body, maxScreenshotRawBytes+1))
}

func (c *clusterKubeVirtClient) DialVNC(ctx context.Context, namespace, name string) (VNCStream, error) {
	return dialKubeVirtVNC(ctx, c.config, namespace, name)
}

func vmSubresourcePath(namespace, name string, subresources ...string) string {
	return strings.Join(append([]string{
		subresourceAPIPath, "namespaces", namespace, "virtualmachines", name,
	}, subresources...), "/")
}

func vmiSubresourcePath(namespace, name string, subresources ...string) string {
	return strings.Join(append([]string{
		subresourceAPIPath, "namespaces", namespace, "virtualmachineinstances", name,
	}, subresources...), "/")
}

// vncOpenError carries the upstream HTTP status of a failed VNC upgrade so
// the handler can separate "desktop not ready" from "gateway credential
// broken" without re-parsing the error text.
type vncOpenError struct {
	statusCode int
	err        error
}

func (e *vncOpenError) Error() string      { return e.err.Error() }
func (e *vncOpenError) Unwrap() error      { return e.err }
func (e *vncOpenError) GetStatusCode() int { return e.statusCode }

// vncDialRoundTripper turns the websocket dial into a round trip so
// rest.HTTPWrappersForConfig can apply the ServiceAccount credential exactly
// as it would to any other Kubernetes request. The round trip stays open for
// the life of the stream: returning early would let the transport tear the
// connection down under the relay.
type vncDialRoundTripper struct {
	dialer     *websocket.Dialer
	connection chan<- *websocket.Conn
	done       <-chan struct{}
}

func (d *vncDialRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	connection, response, err := d.dialer.DialContext(request.Context(), request.URL.String(), request.Header)
	if err != nil {
		return response, err
	}
	select {
	case d.connection <- connection:
	case <-request.Context().Done():
		_ = connection.Close()
		return response, request.Context().Err()
	}
	<-d.done
	_ = connection.Close()
	return response, nil
}

// dialKubeVirtVNC opens the RFB stream of one VMI. TLS material and auth come
// from the same rest.Config the typed clients use; gorilla/websocket performs
// the upgrade because KubeVirt's VNC subresource is a websocket endpoint and
// client-go has no websocket verb.
func dialKubeVirtVNC(ctx context.Context, config *rest.Config, namespace, name string) (VNCStream, error) {
	if config == nil {
		return nil, errors.New("KubeVirt REST config is unavailable")
	}
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, fmt.Errorf("build KubeVirt VNC TLS config: %w", err)
	}
	proxy := http.ProxyFromEnvironment
	if config.Proxy != nil {
		proxy = config.Proxy
	}
	target, err := vncURL(config.Host, namespace, name)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	connections := make(chan *websocket.Conn)
	dialer := &websocket.Dialer{
		Proxy:            proxy,
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: vncHandshakeTimeout,
		WriteBufferSize:  websocketBufferSize,
		ReadBufferSize:   websocketBufferSize,
		Subprotocols:     []string{vncSubprotocol},
	}
	roundTripper, err := rest.HTTPWrappersForConfig(config, &vncDialRoundTripper{
		dialer: dialer, connection: connections, done: done,
	})
	if err != nil {
		return nil, fmt.Errorf("build KubeVirt VNC transport: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build KubeVirt VNC request: %w", err)
	}

	errorsFromDial := make(chan error, 1)
	go func() {
		response, roundTripErr := roundTripper.RoundTrip(request)
		if roundTripErr == nil {
			errorsFromDial <- nil
			return
		}
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
			roundTripErr = enrichVNCDialError(roundTripErr, response)
		}
		errorsFromDial <- &vncOpenError{statusCode: statusCode, err: roundTripErr}
	}()

	select {
	case connection := <-connections:
		stream := newWebSocketVNCStream(connection, done)
		if err := ctx.Err(); err != nil {
			_ = stream.Close()
			return nil, err
		}
		return stream, nil
	case dialErr := <-errorsFromDial:
		// DialContext may report its socket timeout concurrently with the
		// caller context. Preserve the caller-visible cancellation contract.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var timedOut interface{ Timeout() bool }
		if deadline, ok := ctx.Deadline(); ok &&
			errors.As(dialErr, &timedOut) && timedOut.Timeout() &&
			time.Until(deadline) <= 10*time.Millisecond {
			return nil, context.DeadlineExceeded
		}
		if dialErr == nil {
			return nil, errors.New("KubeVirt VNC transport ended before a connection was established")
		}
		return nil, dialErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// vncURL builds the VNC subresource endpoint from the API server host,
// switching to the websocket scheme the dialer requires.
func vncURL(host, namespace, name string) (*url.URL, error) {
	if strings.TrimSpace(host) == "" {
		return nil, errors.New("KubeVirt REST config has no host")
	}
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse Kubernetes API host: %w", err)
	}
	switch parsed.Scheme {
	case "https", "wss":
		parsed.Scheme = "wss"
	case "http", "ws":
		parsed.Scheme = "ws"
	case "":
		parsed, err = url.Parse("wss://" + host)
		if err != nil {
			return nil, fmt.Errorf("parse Kubernetes API host: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported Kubernetes API scheme %q", parsed.Scheme)
	}
	parsed.Path = vmiSubresourcePath(namespace, name, "vnc")
	parsed.RawQuery = url.Values{"preserveSession": {"true"}}.Encode()
	return parsed, nil
}

// enrichVNCDialError folds the upgrade response's status line and a bounded
// body excerpt into the error, because gorilla reports only "bad handshake".
func enrichVNCDialError(cause error, response *http.Response) error {
	var excerpt string
	if response.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		_ = response.Body.Close()
		excerpt = strings.TrimSpace(string(body))
	}
	if excerpt == "" {
		return fmt.Errorf("%w (%s)", cause, response.Status)
	}
	return fmt.Errorf("%w (%s: %s)", cause, response.Status, excerpt)
}

// websocketVNCStream adapts one websocket connection to the RFB byte stream
// the relay copies. RFB has no framing of its own, so KubeVirt may split the
// stream across binary frames at arbitrary offsets.
type websocketVNCStream struct {
	conn      *websocket.Conn
	reader    binaryWebSocketReader
	writer    binaryWebSocketWriter
	done      chan struct{}
	closeOnce sync.Once
}

func newWebSocketVNCStream(conn *websocket.Conn, done chan struct{}) *websocketVNCStream {
	return &websocketVNCStream{
		conn:   conn,
		reader: binaryWebSocketReader{conn: conn},
		writer: binaryWebSocketWriter{conn: conn},
		done:   done,
	}
}

func (s *websocketVNCStream) Read(buffer []byte) (int, error) { return s.reader.Read(buffer) }

func (s *websocketVNCStream) Write(payload []byte) (int, error) { return s.writer.Write(payload) }

// Close ends the stream and releases the round trip that carries it, so the
// transport goroutine cannot outlive the relay.
func (s *websocketVNCStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
		err = s.conn.Close()
	})
	return err
}

// binaryWebSocketReader reassembles the RFB stream across binary frames.
// Non-binary frames are skipped, and a close frame is the stream's EOF.
type binaryWebSocketReader struct {
	conn    *websocket.Conn
	current io.Reader
}

func (r *binaryWebSocketReader) Read(buffer []byte) (int, error) {
	for {
		if r.current == nil {
			messageType, reader, err := r.conn.NextReader()
			if err != nil {
				return 0, closeAsEOF(err)
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			r.current = reader
		}
		count, err := r.current.Read(buffer)
		if errors.Is(err, io.EOF) {
			r.current = nil
			if count > 0 {
				return count, nil
			}
			continue
		}
		if err != nil {
			return count, closeAsEOF(err)
		}
		return count, nil
	}
}

// closeAsEOF maps an orderly websocket close to io.EOF: a peer that closed
// the RFB stream ended it, it did not fail. Only an orderly close qualifies.
// A severed connection surfaces as a synthetic abnormal-closure error, and
// reporting that as EOF would log a torn-down desktop session as a clean
// release.
func closeAsEOF(err error) error {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) &&
		(closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway) {
		return io.EOF
	}
	return err
}

type binaryWebSocketWriter struct {
	conn *websocket.Conn
}

func (w *binaryWebSocketWriter) Write(payload []byte) (int, error) {
	if err := w.conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		return 0, err
	}
	return len(payload), nil
}
