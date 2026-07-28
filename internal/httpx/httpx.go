// Package httpx provides a single process-wide, connection-pool-tuned
// *http.Transport shared by lema's outbound HTTP clients.
//
// The stdlib default (http.DefaultTransport) caps MaxIdleConnsPerHost at 2. A
// bare &http.Client{Timeout: ...} — the idiom every outbound client here used —
// shares that default, so under any real concurrency, repeated calls to a hot
// host (Vertex, WorkOS, GitHub, Resend) keep tearing down and re-establishing
// TLS instead of reusing a warm keep-alive connection. One shared, tuned
// transport amortizes the handshake across the whole process.
//
// An *http.Transport is explicitly designed to be shared and reused; it holds
// the idle-connection pool, so a single instance behind many clients is the
// correct pattern (a per-client transport would fragment the pool and defeat
// the point). Per-call deadlines still live on each client's own Timeout.
package httpx

import (
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	sharedOnce sync.Once
	shared     *http.Transport
)

// SharedTransport returns the process-wide tuned transport. It mirrors
// http.DefaultTransport's dial/TLS/idle timeouts and raises MaxIdleConnsPerHost
// from the stdlib default of 2 to 32 so bursts to one host reuse connections
// rather than serializing behind two. Safe for concurrent use.
func SharedTransport() *http.Transport {
	sharedOnce.Do(func() {
		shared = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				// 30s dial timeout matches http.DefaultTransport exactly — the
				// only intended divergence from the default is MaxIdleConnsPerHost.
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	})
	return shared
}

// Client returns an *http.Client with the given per-request timeout backed by
// the shared transport. A zero timeout means no client-level deadline (for
// callers that bound each call via context instead).
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: SharedTransport()}
}
