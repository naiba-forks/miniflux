// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package headless

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"miniflux.app/v2/internal/config"
)

func findTestPort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func startObscura(t *testing.T, port int) *exec.Cmd {
	t.Helper()

	binaryPath := testObscuraBinaryPath(t)

	cmd := exec.Command(binaryPath, "serve", "--host", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--stealth")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = obscuraEnvironment()

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start Obscura: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		wsURL, err := launcher.ResolveURL(fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil && wsURL != "" {
			return cmd
		}
		time.Sleep(300 * time.Millisecond)
	}

	cmd.Process.Kill()
	cmd.Wait()
	t.Fatal("Obscura CDP server did not become ready within 15s")
	return nil
}

func connectTestBrowser(t *testing.T, port int) (*rod.Browser, *cdp.WebSocket) {
	t.Helper()

	wsURL, err := launcher.ResolveURL(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Failed to resolve CDP URL: %v", err)
	}

	browser, websocket, err := connectBrowser(wsURL)
	if err != nil {
		t.Fatalf("Failed to connect to Obscura CDP: %v", err)
	}
	return browser, websocket
}

func openTestPage(t *testing.T, browser *rod.Browser, pageURL string) *rod.Page {
	t.Helper()

	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("Failed to create page: %v", err)
	}
	if err := navigatePage(page, pageURL); err != nil {
		closePage(browser, page)
		t.Fatalf("Failed to navigate page: %v", err)
	}
	return page
}

func testObscuraBinaryPath(t *testing.T) string {
	t.Helper()

	binaryPath := os.Getenv("OBSCURA_BINARY_PATH")
	if binaryPath == "" {
		binaryPath = "/usr/bin/obscura"
	}
	if _, err := os.Stat(binaryPath); err != nil {
		t.Skipf("Obscura binary not found at %s, skipping e2e test", binaryPath)
	}
	return binaryPath
}

func configureObscuraForTest(t *testing.T) {
	t.Helper()

	previousOptions := config.Opts
	t.Cleanup(func() {
		config.Opts = previousOptions
	})

	t.Setenv("OBSCURA_BINARY_PATH", testObscuraBinaryPath(t))
	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("Failed to parse test config: %v", err)
	}
	config.Opts = parsedOptions
}

func TestObscuraCDPConnection(t *testing.T) {
	port := findTestPort()
	cmd := startObscura(t, port)
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	wsURL, err := launcher.ResolveURL(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Failed to resolve CDP URL: %v", err)
	}
	t.Logf("CDP WebSocket URL: %s", wsURL)

	browser, websocket := connectTestBrowser(t, port)
	defer closeBrowser(browser, websocket)

	page := openTestPage(t, browser, "https://example.com")
	defer closePage(browser, page)

	result, err := page.Eval(`() => document.title`)
	if err != nil {
		t.Fatalf("Eval document.title failed: %v", err)
	}

	title := result.Value.Str()
	t.Logf("Page title: %q", title)
	if title == "" {
		t.Error("Expected non-empty page title from example.com")
	}
}

func TestReadableContentExtraction(t *testing.T) {
	port := findTestPort()
	cmd := startObscura(t, port)
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	browser, websocket := connectTestBrowser(t, port)
	defer closeBrowser(browser, websocket)

	page := openTestPage(t, browser, "https://example.com")
	defer closePage(browser, page)

	content, err := extractReadableContent(page, "https://example.com")
	if err != nil {
		t.Fatalf("extractReadableContent failed: %v", err)
	}

	t.Logf("Extracted content (%d chars): %.200s", len(content), content)

	if content == "" {
		t.Error("Expected non-empty content from example.com")
	}

	if !strings.Contains(content, "documentation") {
		t.Error("Expected content to contain 'documentation'")
	}
}

func TestFullHTMLExtraction(t *testing.T) {
	port := findTestPort()
	cmd := startObscura(t, port)
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	browser, websocket := connectTestBrowser(t, port)
	defer closeBrowser(browser, websocket)

	page := openTestPage(t, browser, "https://example.com")
	defer closePage(browser, page)

	html, err := extractFullHTML(page)
	if err != nil {
		t.Fatalf("extractFullHTML failed: %v", err)
	}

	t.Logf("Full HTML length: %d", len(html))

	if !strings.Contains(html, "<html") {
		t.Error("Expected full HTML to contain <html tag")
	}
	if !strings.Contains(html, "Example Domain") {
		t.Error("Expected full HTML to contain 'Example Domain'")
	}
}

func TestRenderPageStartsConfiguredObscura(t *testing.T) {
	configureObscuraForTest(t)

	content, err := RenderPage("https://example.com", "", 0)
	if err != nil {
		t.Fatalf("RenderPage failed: %v", err)
	}
	if !strings.Contains(content, "documentation") {
		t.Fatalf("Expected rendered content to contain documentation, got %.200q", content)
	}

	html, err := RenderPageHTML("https://example.com", "", 0)
	if err != nil {
		t.Fatalf("RenderPageHTML failed: %v", err)
	}
	if !strings.Contains(html, "Example Domain") {
		t.Fatalf("Expected rendered HTML to contain Example Domain, got %.200q", html)
	}
}

func TestRedactProxyURL(t *testing.T) {
	tests := map[string]string{
		"":                                    "",
		"http://user:secret@example.com:8080": "http://user:xxxxx@example.com:8080",
		"http://example.com:8080":             "http://example.com:8080",
		"%":                                   "<configured>",
	}

	for input, expected := range tests {
		if actual := redactProxyURL(input); actual != expected {
			t.Errorf("redactProxyURL(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func isProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestProcessCleanupAfterSuccess(t *testing.T) {
	port := findTestPort()
	cmd := startObscura(t, port)
	pid := cmd.Process.Pid
	activeProcessCount.Add(1)

	stopSubprocess(cmd)

	time.Sleep(500 * time.Millisecond)
	if isProcessAlive(pid) {
		t.Errorf("Obscura process %d still alive after stopSubprocess", pid)
	}
}

func TestProcessCleanupAfterCDPDisconnect(t *testing.T) {
	port := findTestPort()
	cmd := startObscura(t, port)
	pid := cmd.Process.Pid
	activeProcessCount.Add(1)

	browser, websocket := connectTestBrowser(t, port)
	page := openTestPage(t, browser, "https://example.com")

	closePage(browser, page)
	closeBrowser(browser, websocket)
	stopSubprocess(cmd)

	time.Sleep(500 * time.Millisecond)
	if isProcessAlive(pid) {
		t.Errorf("Obscura process %d still alive after cleanup", pid)
	}
}

func TestActiveProcessCountAccuracy(t *testing.T) {
	before := ActiveProcessCount()

	port := findTestPort()
	cmd := startObscura(t, port)
	activeProcessCount.Add(1)

	if got := ActiveProcessCount(); got != before+1 {
		t.Errorf("ActiveProcessCount after start: got %d, want %d", got, before+1)
	}

	stopSubprocess(cmd)

	time.Sleep(500 * time.Millisecond)
	if got := ActiveProcessCount(); got != before {
		t.Errorf("ActiveProcessCount after stop: got %d, want %d", got, before)
	}
}

func TestStopSubprocessNilSafe(t *testing.T) {
	stopSubprocess(nil)

	cmd := &exec.Cmd{}
	stopSubprocess(cmd)
}

func TestMultipleSequentialRenders(t *testing.T) {
	before := ActiveProcessCount()

	for i := 0; i < 3; i++ {
		port := findTestPort()
		cmd := startObscura(t, port)
		activeProcessCount.Add(1)

		browser, websocket := connectTestBrowser(t, port)
		page := openTestPage(t, browser, "https://example.com")

		result, _ := page.Eval(`() => document.title`)
		t.Logf("Iteration %d: title=%s", i, result.Value.Str())

		closePage(browser, page)
		closeBrowser(browser, websocket)
		stopSubprocess(cmd)
	}

	time.Sleep(500 * time.Millisecond)
	after := ActiveProcessCount()
	if after != before {
		t.Errorf("Process leak: ActiveProcessCount before=%d after=%d", before, after)
	}

}

func TestNoZombieProcesses(t *testing.T) {
	var pids []int
	for i := 0; i < 3; i++ {
		port := findTestPort()
		cmd := startObscura(t, port)
		pids = append(pids, cmd.Process.Pid)
		activeProcessCount.Add(1)
		stopSubprocess(cmd)
	}

	time.Sleep(1 * time.Second)
	for _, pid := range pids {
		if isProcessAlive(pid) {
			t.Errorf("Zombie process detected: pid %d", pid)
		}
	}
}

func TestObscuraProcessCount(t *testing.T) {
	before := ObscuraProcessCount()

	port := findTestPort()
	cmd := startObscura(t, port)
	activeProcessCount.Add(1)

	during := ObscuraProcessCount()
	t.Logf("ObscuraProcessCount: before=%d during=%d", before, during)
	if during <= before {
		t.Errorf("ObscuraProcessCount did not increase while process running: before=%d during=%d", before, during)
	}

	stopSubprocess(cmd)
	time.Sleep(500 * time.Millisecond)

	after := ObscuraProcessCount()
	t.Logf("ObscuraProcessCount after stop: %d", after)
	if after != before {
		t.Errorf("ObscuraProcessCount not back to baseline: before=%d after=%d", before, after)
	}
}
