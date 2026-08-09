package factory

import (
	"io"
	"net/http"
	"strings"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/crypto"
	"github.com/portainer/portainer/api/http/proxy/factory/docker"
	"github.com/portainer/portainer/api/internal/endpointutils"
	"github.com/portainer/portainer/api/url"
	httperror "github.com/portainer/portainer/pkg/libhttp/error"
	"github.com/portainer/portainer/pkg/libhttp/ssrf"

	"github.com/rs/zerolog/log"
)

func (factory *ProxyFactory) newDockerProxy(endpoint *portainer.Endpoint) (http.Handler, error) {
	if strings.HasPrefix(endpoint.URL, "unix://") || strings.HasPrefix(endpoint.URL, "npipe://") {
		return factory.newDockerLocalProxy(endpoint)
	}

	return factory.newDockerHTTPProxy(endpoint)
}

func (factory *ProxyFactory) newDockerLocalProxy(endpoint *portainer.Endpoint) (http.Handler, error) {
	endpointURL, err := url.ParseURL(endpoint.URL)
	if err != nil {
		return nil, err
	}

	return factory.newOSBasedLocalProxy(endpointURL.Path, endpoint)
}

func (factory *ProxyFactory) newDockerHTTPProxy(endpoint *portainer.Endpoint) (http.Handler, error) {
	rawURL := endpoint.URL
	if endpoint.Type == portainer.EdgeAgentOnDockerEnvironment {
		tunnelAddr, err := factory.reverseTunnelService.TunnelAddr(endpoint)
		if err != nil {
			return nil, err
		}
		rawURL = "http://" + tunnelAddr
	}

	endpointURL, err := url.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}

	endpointURL.Scheme = "http"

	transportParameters := &docker.TransportParameters{
		Endpoint:             endpoint,
		DataStore:            factory.dataStore,
		ReverseTunnelService: factory.reverseTunnelService,
		SignatureService:     factory.signatureService,
		DockerClientFactory:  factory.dockerClientFactory,
	}

	var innerTransport *http.Transport
	if endpoint.TLSConfig.TLS || endpoint.TLSConfig.TLSSkipVerify {
		tlsConfig, err := crypto.CreateTLSConfigurationFromDisk(endpoint.TLSConfig)
		if err != nil {
			return nil, err
		}

		endpointURL.Scheme = "https"

		if endpointutils.IsEdgeEndpoint(endpoint) {
			innerTransport = ssrf.NewInternalTransport(tlsConfig)
		} else {
			innerTransport = ssrf.NewTransport(tlsConfig)
			// This hop reaches either a Portainer agent or a dockerd exposing its TLS
			// port directly. The agent relays the request to the Docker socket as-is:
			// over an HTTP/2 hop its handler receives a non-nil body wrapper even for an
			// empty body, so the relayed request is framed as Transfer-Encoding: chunked,
			// which Docker rejects on POST /containers/{id}/start. Stick to HTTP/1.1 so an
			// empty body stays Content-Length: 0. dockerd itself never speaks HTTP/2, so
			// disabling it is a no-op for the direct case.
			innerTransport.Protocols = ssrf.HTTP1Only()
		}
	} else if endpointutils.IsEdgeEndpoint(endpoint) {
		innerTransport = ssrf.NewInternalTransport(nil)
	} else {
		innerTransport = ssrf.NewTransport(nil)
	}

	dockerTransport, err := docker.NewTransport(transportParameters, innerTransport, factory.gitService, factory.snapshotService)
	if err != nil {
		return nil, err
	}

	proxy := NewSingleHostReverseProxyWithHostHeader(endpointURL)
	proxy.Transport = dockerTransport
	return proxy, nil
}

type dockerLocalProxy struct {
	transport *docker.Transport
}

// ServeHTTP is the http.Handler interface implementation
// for a local (Unix socket or Windows named pipe) Docker proxy.
func (proxy *dockerLocalProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Force URL/domain to http/unixsocket to be able to
	// use http.transport RoundTrip to do the requests via the socket
	r.URL.Scheme = "http"
	r.URL.Host = "unixsocket"

	res, err := proxy.transport.ProxyDockerRequest(r)
	if err != nil {
		code := http.StatusInternalServerError
		if res != nil && res.StatusCode != 0 {
			code = res.StatusCode
		}

		httperror.WriteError(w, code, "Unable to proxy the request via the Docker socket", err)
		return
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			log.Warn().Err(err).Msg("proxy error: failed to close response body")
		}
	}()

	for k, vv := range res.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(res.StatusCode)

	streamResponse(w, res.Body)
}

// streamResponse copies body to w, flushing after every chunk instead of using
// io.Copy. io.Copy buffers into the http.ResponseWriter (~2KB) and only flushes
// when the buffer fills or the handler returns, so low-throughput streaming
// docker responses (container logs follow, events, stats, attach) would arrive
// in multi-second batches instead of live; flushing per chunk delivers them as
// they are produced. Behaviour otherwise matches io.Copy: the n>0 chunk is
// written BEFORE the readErr check so a simultaneous Read -> (n>0, io.EOF)
// still emits the final chunk; a write error or a non-EOF read error breaks the
// loop with a Debug log; and a writer that does not implement http.Flusher
// degrades to the old buffered behaviour rather than panicking.
func streamResponse(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				log.Debug().Err(writeErr).Msg("proxy error")
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Debug().Err(readErr).Msg("proxy error")
			}
			break
		}
	}
}
