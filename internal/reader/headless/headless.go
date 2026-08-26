// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package headless // import "miniflux.app/v2/internal/reader/headless"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	rodutils "github.com/go-rod/rod/lib/utils"

	"miniflux.app/v2/internal/config"
)

// Some lightweight CDP browsers can close their WebSocket in ways go-rod turns
// into a panic inside an internal goroutine. Replace go-rod's panic hook with a
// logging-only stub so a browser-side close cannot take down Miniflux.
func init() {
	rodutils.Panic = func(v any) {
		slog.Warn("headless: suppressed go-rod panic from CDP consumer", slog.Any("value", v))
	}
}

const (
	// cdpConnectTimeout is how long we wait for the Obscura CDP server to
	// become reachable after spawning the subprocess.
	cdpConnectTimeout = 30 * time.Second

	// pageNavigationTimeout caps total time for navigation + JS execution +
	// content extraction per page.
	pageNavigationTimeout = 60 * time.Second

	// healthCheckInterval is how frequently we poll /json/version while waiting
	// for Obscura to start.
	healthCheckInterval = 300 * time.Millisecond

	// shutdownGracePeriod is how long we wait for the process to exit after
	// sending SIGTERM before resorting to SIGKILL.
	shutdownGracePeriod = 3 * time.Second
)

var activeProcessCount atomic.Int64

func ActiveProcessCount() int64 {
	return activeProcessCount.Load()
}

// renderPageWithExtractor starts an ephemeral Obscura subprocess, connects
// via CDP (go-rod), navigates to pageURL, and calls extractFn to obtain content
// from the rendered page.
//
// proxyURL is optional: when non-empty it is forwarded to Obscura via --proxy
// so that the page fetch goes through the specified proxy.
//
// feedID is currently unused but reserved for future per-feed state isolation.
func renderPageWithExtractor(pageURL, proxyURL string, feedID int64, extractFn func(*rod.Page) (string, error)) (string, error) {
	port, err := findFreePort()
	if err != nil {
		return "", fmt.Errorf("headless: %w", err)
	}

	cmd, err := startSubprocess(port, proxyURL)
	if err != nil {
		return "", fmt.Errorf("headless: %w", err)
	}
	defer stopSubprocess(cmd)

	// Wait for Obscura's CDP server to be ready by polling /json/version.
	wsURL, err := waitForCDP(port)
	if err != nil {
		return "", fmt.Errorf("headless: %w", err)
	}

	browser := rod.New().ControlURL(wsURL)
	err = browser.Connect()
	if err != nil {
		return "", fmt.Errorf("headless: CDP connect failed: %w", err)
	}
	defer closeBrowser(browser)

	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return "", fmt.Errorf("headless: failed to create page: %w", err)
	}
	defer func() { _ = page.Close() }()

	// Navigate with timeout.
	err = page.Timeout(pageNavigationTimeout).Navigate(pageURL)
	if err != nil {
		return "", fmt.Errorf("headless: navigation to %q failed: %w", pageURL, err)
	}

	// Wait for the page to finish loading (window.onload).
	err = waitForPageLoad(page)
	if err != nil {
		return "", fmt.Errorf("headless: wait load for %q failed: %w", pageURL, err)
	}

	content, err := extractFn(page)
	if err != nil {
		return "", fmt.Errorf("headless: content extraction for %q failed: %w", pageURL, err)
	}

	return content, nil
}

// waitForPageLoad deliberately avoids rod.Page.WaitLoad. WaitLoad installs a
// cached JavaScript helper object, but Obscura can return null when go-rod
// creates that object through Runtime.callFunctionOn. Polling readyState uses a
// plain function call and works with both Obscura and full Chromium CDP servers.
func waitForPageLoad(page *rod.Page) error {
	timedPage := page.Timeout(pageNavigationTimeout)
	defer timedPage.CancelTimeout()

	return timedPage.Wait(rod.Eval(`() => document.readyState === "complete"`))
}

func closeBrowser(browser *rod.Browser) {
	defer func() {
		if e := recover(); e != nil {
			slog.Warn("headless: panic while closing browser", slog.Any("error", e))
		}
	}()
	browser.Close()
}

// RenderPage renders the page with JS, gets the full HTML, then extracts
// readable article content via Defuddle (node subprocess). Returns cleaned HTML.
func RenderPage(pageURL, proxyURL string, feedID int64) (string, error) {
	return renderPageWithExtractor(pageURL, proxyURL, feedID, func(page *rod.Page) (string, error) {
		return extractReadableContent(page, pageURL)
	})
}

// RenderPageHTML renders the page with JS and returns the full DOM HTML.
// Used by the web scraper to parse JS-rendered listing pages with CSS selectors.
func RenderPageHTML(pageURL, proxyURL string, feedID int64) (string, error) {
	return renderPageWithExtractor(pageURL, proxyURL, feedID, extractFullHTML)
}

// defuddleNodeScript is the inline Node.js script that reads HTML from stdin
// and extracts article content via Defuddle. The defuddle package must be
// installed at /usr/share/miniflux/defuddle (Docker) or findable via NODE_PATH.
const defuddleNodeScript = `
const {Defuddle} = require('/usr/share/miniflux/defuddle/dist/node.js');
const url = process.argv[1] || 'about:blank';
const chunks = [];
process.stdin.on('data', c => chunks.push(c));
process.stdin.on('end', async () => {
  const html = Buffer.concat(chunks).toString();
  const result = await Defuddle(html, url);
  process.stdout.write(JSON.stringify({title: result.title, content: result.content}));
});
`

// extractReadableContent gets rendered HTML, then pipes it to a node subprocess
// running Defuddle for article content extraction.
func extractReadableContent(page *rod.Page, pageURL string) (string, error) {
	htmlResult, err := page.Eval(`() => document.documentElement.outerHTML`)
	if err != nil {
		return "", fmt.Errorf("outerHTML extraction failed: %w", err)
	}
	rawHTML := htmlResult.Value.Str()

	content, err := runDefuddle(rawHTML, pageURL)
	if err != nil {
		slog.Warn("headless: defuddle extraction failed, falling back to innerText",
			slog.Any("error", err),
		)
		innerResult, innerErr := page.Eval(`() => document.body.innerText`)
		if innerErr != nil {
			return "", fmt.Errorf("defuddle and innerText both failed: %w", innerErr)
		}
		return innerResult.Value.Str(), nil
	}
	return content, nil
}

func runDefuddle(rawHTML, pageURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "node", "-e", defuddleNodeScript, pageURL)
	cmd.Stdin = strings.NewReader(rawHTML)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("node defuddle failed: %w (stderr: %s)", err, stderr.String())
	}

	var parsed struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return "", fmt.Errorf("defuddle JSON decode failed: %w", err)
	}
	return parsed.Content, nil
}

// extractFullHTML returns the complete rendered DOM HTML via
// document.documentElement.outerHTML. This preserves DOM structure needed for
// CSS selector parsing in the web scraper.
func extractFullHTML(page *rod.Page) (string, error) {
	result, err := page.Eval(`() => document.documentElement.outerHTML`)
	if err != nil {
		return "", fmt.Errorf("outerHTML eval failed: %w", err)
	}
	return result.Value.Str(), nil
}

func startSubprocess(port int, proxyURL string) (*exec.Cmd, error) {
	binaryPath := config.Opts.ObscuraBinaryPath()

	args := []string{
		"serve",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
	}
	if config.Opts.ObscuraStealth() {
		args = append(args, "--stealth")
	}
	if config.Opts.ObscuraAllowPrivateNetworks() {
		args = append(args, "--allow-private-network")
	}

	if proxyURL != "" {
		args = append(args, "--proxy", proxyURL)
	}

	cmd := exec.Command(binaryPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Env = os.Environ()

	slog.Debug("headless: starting ephemeral Obscura subprocess",
		slog.String("binary", binaryPath),
		slog.Int("port", port),
		slog.String("proxy_url", redactProxyURL(proxyURL)),
		slog.Bool("stealth", config.Opts.ObscuraStealth()),
		slog.Bool("allow_private_networks", config.Opts.ObscuraAllowPrivateNetworks()),
	)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %q: %w", binaryPath, err)
	}
	activeProcessCount.Add(1)

	return cmd, nil
}

func redactProxyURL(proxyURL string) string {
	if proxyURL == "" {
		return ""
	}
	parsedURL, err := url.Parse(proxyURL)
	if err != nil {
		return "<configured>"
	}
	return parsedURL.Redacted()
}

// stopSubprocess sends SIGTERM and waits for graceful exit. Falls back to
// SIGKILL if the process doesn't exit within shutdownGracePeriod.
func stopSubprocess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	defer activeProcessCount.Add(-1)

	// Try graceful termination first.
	_ = cmd.Process.Signal(os.Interrupt)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		return
	case <-time.After(shutdownGracePeriod):
		slog.Warn("headless: Obscura shutdown timed out, force killing")
		_ = cmd.Process.Kill()
		// Reap zombie process.
		<-done
	}
}

// waitForCDP polls /json/version until Obscura responds with a valid
// webSocketDebuggerUrl, or until cdpConnectTimeout elapses.
func waitForCDP(port int) (string, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(cdpConnectTimeout)

	for time.Now().Before(deadline) {
		// launcher.ResolveURL queries /json/version and extracts the WS URL.
		wsURL, err := launcher.ResolveURL(addr)
		if err == nil && wsURL != "" {
			return wsURL, nil
		}
		time.Sleep(healthCheckInterval)
	}

	return "", fmt.Errorf("Obscura CDP not ready on port %d after %s", port, cdpConnectTimeout)
}

func findFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("unable to find free port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port, nil
}

// ObscuraProcessCount scans /proc for running obscura processes.
// Non-zero after all renders complete indicates a resource leak.
func ObscuraProcessCount() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		slog.Warn("headless: unable to scan /proc for obscura processes", slog.Any("error", err))
		return 0
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid := entry.Name()
		if len(pid) == 0 || pid[0] < '1' || pid[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil {
			continue
		}
		if nullIdx := strings.IndexByte(string(cmdline), 0); nullIdx > 0 {
			if strings.Contains(string(cmdline[:nullIdx]), "obscura") {
				count++
			}
		}
	}
	return count
}
