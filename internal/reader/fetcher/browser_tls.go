// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fetcher

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// browserTLSTransport retains the fetcher's proxy selection and guarded dialer.
// DialTLSContext alone is insufficient: net/http uses crypto/tls internally
// after CONNECT, so proxied origins would still receive Go's ClientHello.
type browserTLSTransport struct {
	base         *http.Transport
	disableHTTP2 bool
}

func (t *browserTLSTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" {
		return t.base.RoundTrip(request)
	}
	proxyURL, err := t.base.Proxy(request)
	if err != nil {
		return nil, err
	}
	originURL := *request.URL
	originURL.Host, err = httpguts.PunycodeHostPort(originURL.Host)
	if err != nil {
		return nil, err
	}
	address := net.JoinHostPort(originURL.Hostname(), originURL.Port())
	if originURL.Port() == "" {
		address = net.JoinHostPort(originURL.Hostname(), "443")
	}
	connection, err := t.dialOrigin(request.Context(), address, proxyURL)
	if err != nil {
		return nil, err
	}

	// The request timeout must cover CONNECT, TLS and body reads, including
	// servers that never complete their handshake. Stop the callback on EOF
	// or Close so successful requests don't retain connections until timeout.
	stopCancellation := context.AfterFunc(request.Context(), func() { _ = connection.Close() })
	cleanup := sync.OnceFunc(func() {
		stopCancellation()
		_ = connection.Close()
	})
	response, err := t.roundTripTLS(request, connection, originURL.Hostname())
	if err != nil {
		cleanup()
		return nil, err
	}
	response.Body = &browserTLSResponseBody{ReadCloser: response.Body, cleanup: cleanup}
	return response, nil
}

func (t *browserTLSTransport) roundTripTLS(request *http.Request, connection net.Conn, serverName string) (*http.Response, error) {
	tlsConfig := &utls.Config{ServerName: serverName}
	if t.base.TLSClientConfig != nil {
		tlsConfig.RootCAs = t.base.TLSClientConfig.RootCAs
		tlsConfig.InsecureSkipVerify = t.base.TLSClientConfig.InsecureSkipVerify
	}
	tlsConnection := utls.UClient(connection, tlsConfig, utls.HelloChrome_Auto)
	if t.disableHTTP2 {
		// Browser presets own the ALPN extension; setting Config.NextProtos
		// alone does not stop the Chrome preset from advertising h2.
		if err := tlsConnection.BuildHandshakeState(); err != nil {
			return nil, err
		}
		for _, extension := range tlsConnection.Extensions {
			if alpn, ok := extension.(*utls.ALPNExtension); ok {
				alpn.AlpnProtocols = []string{"http/1.1"}
			}
		}
		// ALPS carries HTTP/2 settings and must not advertise a disabled protocol.
		tlsConnection.Extensions = slices.DeleteFunc(tlsConnection.Extensions, func(extension utls.TLSExtension) bool {
			switch extension.(type) {
			case *utls.ApplicationSettingsExtension, *utls.ApplicationSettingsExtensionNew:
				return true
			}
			return false
		})
	}
	if err := tlsConnection.HandshakeContext(request.Context()); err != nil {
		return nil, err
	}

	var response *http.Response
	var err error
	state := tlsConnection.ConnectionState()
	switch state.NegotiatedProtocol {
	case "h2":
		// uTLS connections are not *crypto/tls.Conn. Go 1.26's HTTP
		// transport cannot dispatch their ALPN result, so use x/net/http2
		// for h2 and net/http for HTTP/1.1 on the negotiated connection.
		http2Transport := &http2.Transport{DisableCompression: true}
		http2Connection, connectErr := http2Transport.NewClientConn(tlsConnection)
		if connectErr != nil {
			return nil, connectErr
		}
		defer http2Connection.SetDoNotReuse()
		response, err = http2Connection.RoundTrip(request)
	case "", "http/1.1":
		http1Transport := &http.Transport{
			DisableKeepAlives:  true,
			DisableCompression: true,
			DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				return tlsConnection, nil
			},
		}
		response, err = http1Transport.RoundTrip(request)
	default:
		return nil, fmt.Errorf("fetcher: unsupported TLS application protocol %q", state.NegotiatedProtocol)
	}
	if err != nil {
		return nil, err
	}
	response.TLS = &tls.ConnectionState{
		Version:                     state.Version,
		HandshakeComplete:           state.HandshakeComplete,
		DidResume:                   state.DidResume,
		CipherSuite:                 state.CipherSuite,
		NegotiatedProtocol:          state.NegotiatedProtocol,
		NegotiatedProtocolIsMutual:  state.NegotiatedProtocolIsMutual,
		ServerName:                  state.ServerName,
		PeerCertificates:            state.PeerCertificates,
		VerifiedChains:              state.VerifiedChains,
		SignedCertificateTimestamps: state.SignedCertificateTimestamps,
		OCSPResponse:                state.OCSPResponse,
		TLSUnique:                   state.TLSUnique,
	}
	return response, nil
}

func (t *browserTLSTransport) dialOrigin(ctx context.Context, address string, proxyURL *url.URL) (net.Conn, error) {
	if proxyURL == nil {
		return t.base.DialContext(ctx, "tcp", address)
	}
	connection, err := t.dialProxy(ctx, address, proxyURL)
	if err != nil {
		return nil, &net.OpError{Op: "proxyconnect", Net: "tcp", Err: err}
	}
	return connection, nil
}

func (t *browserTLSTransport) dialProxy(ctx context.Context, address string, proxyURL *url.URL) (net.Conn, error) {
	if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
		var auth *proxy.Auth
		if proxyURL.User != nil {
			password, _ := proxyURL.User.Password()
			auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", normalizeProxyDialAddress(proxyURL), auth, browserTLSProxyDialer{t.base.DialContext})
		if err != nil {
			return nil, err
		}
		return dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", address)
	}
	if proxyURL.Scheme != "" && proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, fmt.Errorf("fetcher: unsupported proxy scheme %q", proxyURL.Scheme)
	}
	connection, err := t.base.DialContext(ctx, "tcp", normalizeProxyDialAddress(proxyURL))
	if err != nil {
		return nil, err
	}
	// CONNECT is parsed with the standard HTTP library, but must observe the
	// request deadline even if the proxy accepts TCP and never replies.
	rawConnection := connection
	stopCancellation := context.AfterFunc(ctx, func() { _ = rawConnection.Close() })
	defer stopCancellation()
	connected := false
	defer func() {
		if !connected {
			_ = rawConnection.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if proxyURL.Scheme == "https" {
		proxyTLSConfig := &tls.Config{ServerName: proxyURL.Hostname(), NextProtos: []string{"http/1.1"}}
		if t.base.TLSClientConfig != nil {
			proxyTLSConfig.RootCAs = t.base.TLSClientConfig.RootCAs
			proxyTLSConfig.InsecureSkipVerify = t.base.TLSClientConfig.InsecureSkipVerify
		}
		proxyConnection := tls.Client(connection, proxyTLSConfig)
		if err := proxyConnection.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		connection = proxyConnection
	}
	connectRequest := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: address},
		Host:   address,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		credentials := proxyURL.User.Username() + ":" + password
		connectRequest.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	if err := connectRequest.Write(connection); err != nil {
		return nil, err
	}
	// Bound the CONNECT headers before giving the buffered reader to TLS.
	headerReader := &io.LimitedReader{R: connection, N: 1 << 20}
	reader := bufio.NewReader(headerReader)
	response, err := http.ReadResponse(reader, connectRequest)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CONNECT tunnel: %s", response.Status)
	}
	headerReader.N = 1<<63 - 1
	_ = connection.SetDeadline(time.Time{})
	connected = true
	return &browserTLSBufferedConnection{Conn: connection, reader: reader}, nil
}

type browserTLSProxyDialer struct {
	dialContext func(context.Context, string, string) (net.Conn, error)
}

func (d browserTLSProxyDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d browserTLSProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dialContext(ctx, network, address)
}

type browserTLSBufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *browserTLSBufferedConnection) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}

type browserTLSResponseBody struct {
	io.ReadCloser
	cleanup func()
}

func (b *browserTLSResponseBody) Read(buffer []byte) (int, error) {
	n, err := b.ReadCloser.Read(buffer)
	if err != nil {
		b.cleanup()
	}
	return n, err
}

func (b *browserTLSResponseBody) Close() error {
	defer b.cleanup()
	return b.ReadCloser.Close()
}
