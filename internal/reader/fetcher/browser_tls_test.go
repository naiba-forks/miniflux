// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fetcher

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func clearFetcherProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(key, "")
	}
}

// A real CONNECT tunnel ensures we inspect the origin handshake, not merely
// the connection to the proxy. Proxy credentials must never reach the origin.
func newBrowserTLSProxy(t *testing.T, targetAddress string, secure bool) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "feed.example:443" {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
			http.Error(w, "unexpected CONNECT target", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Proxy-Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("reader:secret")) {
			t.Errorf("proxy authorization = %q", got)
			http.Error(w, "proxy authorization required", http.StatusProxyAuthRequired)
			return
		}
		upstream, err := net.Dial("tcp", targetAddress)
		if err != nil {
			t.Error(err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := buffer.Flush(); err != nil {
			t.Error(err)
			return
		}
		copied := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, buffer)
			_ = upstream.Close()
			close(copied)
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-copied
	}))
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server
}

func TestRequestBuilderBrowserTLSHandshake(t *testing.T) {
	clearFetcherProxyEnvironment(t)
	configureFetcherAllowPrivateNetworksOption(t, "1")
	for _, route := range []string{"direct", "http_proxy", "https_proxy", "environment_proxy", "socks5", "socks5h", "http1_origin"} {
		for _, disableHTTP2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disable_http2=%t", route, disableHTTP2), func(t *testing.T) {
				hellos := make(chan tls.ClientHelloInfo, 1)
				origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if got := r.Header.Get("Proxy-Authorization"); got != "" {
						t.Errorf("proxy credentials leaked to origin: %q", got)
					}
					_, _ = io.WriteString(w, "<rss>browser TLS feed</rss>")
				}))
				origin.EnableHTTP2 = route != "http1_origin"
				origin.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					hellos <- *hello
					return nil, nil
				}}
				origin.StartTLS()
				defer origin.Close()
				builder := NewRequestBuilder().IgnoreTLSErrors(true).DisableHTTP2(disableHTTP2)
				requestURL := origin.URL
				proxied := route != "direct" && route != "http1_origin"
				if proxied {
					var proxyAddress string
					if route == "socks5" || route == "socks5h" {
						proxyAddress = route + "://" + newBrowserTLSSOCKSProxy(t, origin.Listener.Addr().String())
					} else {
						proxyAddress = newBrowserTLSProxy(t, origin.Listener.Addr().String(), route == "https_proxy").URL
					}
					proxyURL, _ := url.Parse(proxyAddress)
					proxyURL.User = url.UserPassword("reader", "secret")
					requestURL = "https://feed.example/feed/"
					if route == "environment_proxy" {
						t.Setenv("HTTPS_PROXY", proxyURL.String())
					} else {
						builder.WithCustomApplicationProxyURL(proxyURL).UseCustomApplicationProxyURL(true)
					}
				}
				response, err := builder.ExecuteRequest(requestURL)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != "<rss>browser TLS feed</rss>" || response.StatusCode != http.StatusOK {
					t.Fatalf("response: status=%d body=%q err=%v", response.StatusCode, body, err)
				}
				wantProtocol := 2
				if disableHTTP2 || !origin.EnableHTTP2 {
					wantProtocol = 1
				}
				if response.ProtoMajor != wantProtocol {
					t.Errorf("HTTP version = %s, want HTTP/%d", response.Proto, wantProtocol)
				}
				if response.TLS == nil || !response.TLS.HandshakeComplete {
					t.Error("response lost TLS connection state")
				}
				select {
				case hello := <-hellos:
					// Chrome offers GREASE cipher suites and groups; Go's default
					// ClientHello does not. Check both on the actual wire handshake.
					if !slices.ContainsFunc(hello.CipherSuites, isGREASEValue) || !slices.ContainsFunc(hello.SupportedCurves, func(group tls.CurveID) bool { return isGREASEValue(uint16(group)) }) {
						t.Errorf("origin received non-browser TLS handshake: ciphers=%x groups=%x", hello.CipherSuites, hello.SupportedCurves)
					}
					if disableHTTP2 && slices.Contains(hello.SupportedProtos, "h2") {
						t.Errorf("HTTP/2 disabled but advertised via ALPN: %v", hello.SupportedProtos)
					}
					if proxied && hello.ServerName != "feed.example" {
						t.Errorf("origin SNI = %q", hello.ServerName)
					}
				default:
					t.Fatal("origin received no TLS handshake")
				}
			})
		}
	}
}

func newBrowserTLSSOCKSProxy(t *testing.T, targetAddress string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(client)
		readBytes := func(count int) []byte {
			buffer := make([]byte, count)
			_, _ = io.ReadFull(reader, buffer)
			return buffer
		}
		greeting := readBytes(2)
		methods := readBytes(int(greeting[1]))
		if greeting[0] != 5 || !slices.Contains(methods, byte(2)) {
			t.Errorf("SOCKS5 client did not offer password authentication: %v %v", greeting, methods)
			return
		}
		_, _ = client.Write([]byte{5, 2})
		auth := readBytes(2)
		username := string(readBytes(int(auth[1])))
		passwordLength := readBytes(1)[0]
		password := string(readBytes(int(passwordLength)))
		if auth[0] != 1 || username != "reader" || password != "secret" {
			t.Errorf("unexpected SOCKS5 credentials: %q %q", username, password)
			return
		}
		_, _ = client.Write([]byte{1, 0})
		command := readBytes(4)
		if !slices.Equal(command, []byte{5, 1, 0, 3}) {
			t.Errorf("expected SOCKS5 CONNECT with remote DNS, got %v", command)
			return
		}
		hostLength := readBytes(1)[0]
		host := string(readBytes(int(hostLength)))
		port := readBytes(2)
		if host != "feed.example" || !slices.Equal(port, []byte{1, 187}) {
			t.Errorf("unexpected SOCKS5 target: %s %v", host, port)
			return
		}
		upstream, err := net.Dial("tcp", targetAddress)
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.Close()
		_, _ = client.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
		copied := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, reader)
			_ = upstream.Close()
			close(copied)
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-copied
	}()
	return listener.Addr().String()
}

func TestRequestBuilderBrowserTLSRedirectRechecksPrivateNetwork(t *testing.T) {
	clearFetcherProxyEnvironment(t)
	configureFetcherAllowPrivateNetworksOption(t, "0")
	privateOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect bypassed private-network protection")
	}))
	defer privateOrigin.Close()
	redirectOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, privateOrigin.URL, http.StatusFound)
	}))
	defer redirectOrigin.Close()
	proxy := newBrowserTLSProxy(t, redirectOrigin.Listener.Addr().String(), false)
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("reader", "secret")
	t.Setenv("HTTPS_PROXY", proxyURL.String())
	_, err := NewRequestBuilder().IgnoreTLSErrors(true).ExecuteRequest("https://feed.example/feed/")
	if !errors.Is(err, ErrPrivateNetworkHost) {
		t.Fatalf("expected redirected direct request to be blocked, got %v", err)
	}
}

func TestRequestBuilderBrowserTLSProxyConnectTimeout(t *testing.T) {
	clearFetcherProxyEnvironment(t)
	configureFetcherAllowPrivateNetworksOption(t, "1")
	closed := make(chan struct{}, maxProxyRetries+1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("proxy request method = %s", r.Method)
		}
		<-r.Context().Done()
		closed <- struct{}{}
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	_, err := NewRequestBuilder().WithTimeout(100 * time.Millisecond).ExecuteRequest("https://feed.example/feed/")
	var timeoutError net.Error
	if !errors.As(err, &timeoutError) || !timeoutError.Timeout() {
		t.Fatalf("expected CONNECT timeout, got %v", err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("CONNECT connection leaked after timeout")
	}
}

func isGREASEValue(value uint16) bool {
	return value&0x0f0f == 0x0a0a && value>>8 == value&0xff
}

func TestRequestBuilderBrowserTLSRejectsUntrustedCertificate(t *testing.T) {
	clearFetcherProxyEnvironment(t)
	configureFetcherAllowPrivateNetworksOption(t, "1")
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached origin despite untrusted certificate")
	}))
	defer origin.Close()
	_, err := NewRequestBuilder().ExecuteRequest(origin.URL)
	if err == nil || !strings.Contains(err.Error(), "certificate signed by unknown authority") {
		t.Fatalf("expected certificate validation failure, got %v", err)
	}
}

func TestRequestBuilderBrowserTLSTimeoutClosesHandshakeConnection(t *testing.T) {
	clearFetcherProxyEnvironment(t)
	configureFetcherAllowPrivateNetworksOption(t, "1")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = io.Copy(io.Discard, connection)
		closed <- err
	}()
	_, err = NewRequestBuilder().WithTimeout(100 * time.Millisecond).ExecuteRequest("https://" + listener.Addr().String())
	var timeoutError net.Error
	if !errors.As(err, &timeoutError) || !timeoutError.Timeout() {
		t.Fatalf("expected TLS handshake timeout, got %v", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("TLS connection was not cleanly closed on timeout: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TLS connection leaked after request timeout")
	}
}

func TestRequestBuilderBrowserTLSBodyTimeout(t *testing.T) {
	clearFetcherProxyEnvironment(t)
	configureFetcherAllowPrivateNetworksOption(t, "1")
	for _, disableHTTP2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("disable_http2=%t", disableHTTP2), func(t *testing.T) {
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "<rss>")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			origin.EnableHTTP2 = true
			origin.StartTLS()
			defer origin.Close()
			response, err := NewRequestBuilder().IgnoreTLSErrors(true).DisableHTTP2(disableHTTP2).
				WithTimeout(200 * time.Millisecond).ExecuteRequest(origin.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if string(body) != "<rss>" {
				t.Fatalf("partial body = %q", body)
			}
			var timeoutError net.Error
			if !errors.As(err, &timeoutError) || !timeoutError.Timeout() {
				t.Fatalf("expected response body timeout, got %v", err)
			}
		})
	}
}

func TestRequestBuilderBrowserTLSPrivateNetworkPolicy(t *testing.T) {
	clearFetcherProxyEnvironment(t)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("private origin was reached despite SSRF protection")
	}))
	defer origin.Close()
	for _, route := range []string{"direct", "feed_proxy"} {
		t.Run(route, func(t *testing.T) {
			configureFetcherPrivateNetworkOptions(t, "0", "0")
			builder := NewRequestBuilder().IgnoreTLSErrors(true)
			requestURL := origin.URL
			if route == "feed_proxy" {
				builder.WithCustomFeedProxyURL(origin.URL)
				requestURL = "https://feed.example/feed/"
			}
			_, err := builder.ExecuteRequest(requestURL)
			if !errors.Is(err, ErrPrivateNetworkHost) {
				t.Fatalf("expected SSRF rejection, got %v", err)
			}
		})
	}
}
