// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package headless

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObscuraRendersJavaScriptArticle(t *testing.T) {
	t.Setenv("OBSCURA_ALLOW_PRIVATE_NETWORKS", "1")
	configureObscuraForTest(t)

	const paragraph = "The observatory recorded a new meteor shower above the northern mountains. Researchers compared the telescope measurements and published their findings for the local astronomy community."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>Observatory report</title></head><body><main><article id="report"></article></main><script>document.getElementById('report').innerHTML = '<h1>Observatory report</h1>' + '<p>%s</p>'.repeat(6);</script></body></html>`, paragraph)
	}))
	defer server.Close()

	before := ActiveProcessCount()
	html, err := RenderPageHTML(server.URL, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "<h1>Observatory report</h1><p>"+paragraph) {
		t.Fatalf("Rendered DOM does not contain the JavaScript-generated article: %.300s", html)
	}
	// Check Defuddle directly so the innerText fallback cannot conceal a broken
	// extraction dependency when validating a new browser release.
	content, err := runDefuddle(html, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "<p>"+paragraph+"</p>") || strings.Contains(content, "<script") {
		t.Fatalf("Expected extracted article paragraphs without scripts, got %.300s", content)
	}
	if after := ActiveProcessCount(); after != before {
		t.Fatalf("Browser process count changed from %d to %d after rendering", before, after)
	}
}
