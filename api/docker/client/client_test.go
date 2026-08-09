package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/pkg/fips"
	"github.com/portainer/portainer/pkg/libhttp/ssrf"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
)

func TestHttpClient(t *testing.T) {
	t.Parallel()
	fips.InitFIPS(false)

	// Valid TLS configuration
	endpoint := &portainer.Endpoint{}
	endpoint.TLSConfig = portainer.TLSConfiguration{TLS: true}

	cli, err := httpClient(endpoint, nil)
	require.NoError(t, err)
	require.NotNil(t, cli)

	// Invalid TLS configuration
	endpoint.TLSConfig.TLSCertPath = "/invalid/path/client.crt"
	endpoint.TLSConfig.TLSKeyPath = "/invalid/path/client.key"

	cli, err = httpClient(endpoint, nil)
	require.Error(t, err)
	require.Nil(t, cli)
}

func TestHttpClientDisablesHTTP2OverTLS(t *testing.T) {
	t.Parallel()
	fips.InitFIPS(false)

	// A TLS endpoint is either a Portainer agent or a dockerd exposing its TLS
	// port directly. The agent relays our request to the Docker socket verbatim,
	// and HTTP/2 on that hop corrupts empty bodies (see
	// TestHttpClientStartsContainerThroughHTTP2CapableAgent). dockerd never speaks
	// HTTP/2 itself, so offering HTTP/1.1 only is harmless there and fixes the
	// agent case.
	tlsEndpoint := &portainer.Endpoint{}
	tlsEndpoint.TLSConfig = portainer.TLSConfiguration{TLS: true}

	tlsCli, err := httpClient(tlsEndpoint, nil)
	require.NoError(t, err)

	tlsTransport, ok := tlsCli.Transport.(*NodeNameTransport)
	require.True(t, ok)
	require.NotNil(t, tlsTransport.Protocols, "Protocols must be set: a nil value means HTTP/1.1 + HTTP/2")
	require.False(t, tlsTransport.Protocols.HTTP2())
	require.True(t, tlsTransport.Protocols.HTTP1())

	// Plain HTTP endpoints keep the upstream default (Protocols unset): Go's client
	// never negotiates h2c on its own, so there is nothing to disable there.
	plainEndpoint := &portainer.Endpoint{}
	plainEndpoint.TLSConfig = portainer.TLSConfiguration{TLS: false}

	plainCli, err := httpClient(plainEndpoint, nil)
	require.NoError(t, err)

	plainTransport, ok := plainCli.Transport.(*NodeNameTransport)
	require.True(t, ok)
	require.Nil(t, plainTransport.Protocols)
}

// dockerEmptyBodyError is the message the Docker daemon returns when a
// /containers/{id}/start request carries a body it cannot prove to be empty.
const dockerEmptyBodyError = "starting container with non-empty request body was deprecated since API v1.22 and removed in v1.24"

// TestHttpClientStartsContainerThroughHTTP2CapableAgent reproduces the whole
// broken chain end to end: Docker SDK -> our http.Client -> an HTTP/2-capable
// Portainer agent -> the Docker daemon.
//
// ContainerStart sends no body at all. When the agent hop runs over HTTP/2 its
// handler still receives a non-nil Body wrapper with ContentLength == 0, and
// relaying that request through a client transport makes Request.outgoingLength()
// return -1, i.e. Transfer-Encoding: chunked - which the daemon rejects. Pinning
// the transport to HTTP/1.1 keeps the empty body as Content-Length: 0.
func TestHttpClientStartsContainerThroughHTTP2CapableAgent(t *testing.T) {
	t.Parallel()
	fips.InitFIPS(false)

	var mu sync.Mutex
	var agentProto string
	var daemonCalled bool
	var daemonContentLength int64
	var daemonTransferEncoding []string

	// The Docker daemon: repeats moby's guard on POST /containers/{id}/start.
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/start") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		mu.Lock()
		daemonCalled = true
		daemonContentLength = r.ContentLength
		daemonTransferEncoding = r.TransferEncoding
		mu.Unlock()

		if r.ContentLength > 7 || r.ContentLength == -1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"` + dockerEmptyBodyError + `"}`))

			return
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer daemon.Close()

	daemonURL, err := url.Parse(daemon.URL)
	require.NoError(t, err)

	// The Portainer agent: its LocalProxy retargets the *server* request and hands
	// it straight to a client transport, without normalising the body.
	relay := ssrf.NewTransport(nil)
	defer relay.CloseIdleConnections()

	agent := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		agentProto = r.Proto
		mu.Unlock()

		r.URL.Scheme = "http"
		r.URL.Host = daemonURL.Host

		res, err := relay.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		// This runs in the test server's goroutine, so it must not call require.
		defer func() { _ = res.Body.Close() }()

		for k, vv := range res.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}

		w.WriteHeader(res.StatusCode)
		_, _ = io.Copy(w, res.Body)
	}))
	agent.EnableHTTP2 = true
	agent.StartTLS()
	defer agent.Close()

	// Portainer's side, wired exactly like createAgentClient does.
	endpoint := &portainer.Endpoint{URL: "tcp://" + agent.Listener.Addr().String()}
	endpoint.TLSConfig = portainer.TLSConfiguration{TLS: true, TLSSkipVerify: true}

	httpCli, err := httpClient(endpoint, nil)
	require.NoError(t, err)

	cli, err := client.NewClientWithOpts(
		client.WithHost(endpoint.URL),
		client.WithHTTPClient(httpCli),
		client.WithScheme("https"),
	)
	require.NoError(t, err)
	defer func() {
		err := cli.Close()
		require.NoError(t, err)
	}()

	err = cli.ContainerStart(context.Background(), "probe", container.StartOptions{})

	// ContainerStart has returned, so both hops are done writing.
	mu.Lock()
	proto, called := agentProto, daemonCalled
	contentLength, transferEncoding := daemonContentLength, daemonTransferEncoding
	mu.Unlock()

	require.NoError(t, err, "agent hop protocol: %s, daemon saw Content-Length=%d Transfer-Encoding=%v", proto, contentLength, transferEncoding)
	require.Equal(t, "HTTP/1.1", proto, "the agent hop must not be negotiated as HTTP/2")
	require.True(t, called, "the daemon handler must have been reached, otherwise the assertions below are vacuous")
	require.Equal(t, int64(0), contentLength, "the daemon must see an empty body, not a chunked one")
	require.Empty(t, transferEncoding)
}
