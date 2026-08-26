// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package headless

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

type recordedCDPCall struct {
	sessionID string
	method    string
	params    interface{}
}

type recordingCDPClient struct {
	response []byte
	err      error
	calls    []recordedCDPCall
}

func (c *recordingCDPClient) Call(_ context.Context, sessionID, method string, params interface{}) ([]byte, error) {
	c.calls = append(c.calls, recordedCDPCall{sessionID: sessionID, method: method, params: params})
	return c.response, c.err
}

func TestObscuraLifecycleUsesCompatibleCDPCommands(t *testing.T) {
	client := &recordingCDPClient{response: []byte(`{}`)}
	ctx := context.Background()

	if err := navigateWithCDP(ctx, client, "session-1", "https://example.com/article"); err != nil {
		t.Fatalf("navigateWithCDP failed: %v", err)
	}
	if err := closeTargetWithCDP(ctx, client, "target-1"); err != nil {
		t.Fatalf("closeTargetWithCDP failed: %v", err)
	}
	if err := closeBrowserWithCDP(ctx, client); err != nil {
		t.Fatalf("closeBrowserWithCDP failed: %v", err)
	}

	methods := make([]string, 0, len(client.calls))
	for _, call := range client.calls {
		methods = append(methods, call.method)
	}
	if expected := []string{"Page.navigate", "Target.closeTarget", "Browser.close"}; !reflect.DeepEqual(methods, expected) {
		t.Fatalf("CDP methods = %v, want %v", methods, expected)
	}

	navigateCall := client.calls[0]
	if navigateCall.sessionID != "session-1" {
		t.Fatalf("Page.navigate session ID = %q, want session-1", navigateCall.sessionID)
	}
	params, ok := navigateCall.params.(obscuraNavigateParams)
	if !ok {
		t.Fatalf("Page.navigate params have type %T", navigateCall.params)
	}
	if params.URL != "https://example.com/article" || params.WaitUntil != "load" {
		t.Fatalf("Page.navigate params = %+v", params)
	}

	closeTargetCall := client.calls[1]
	if closeTargetCall.sessionID != "" {
		t.Fatalf("Target.closeTarget session ID = %q, want empty", closeTargetCall.sessionID)
	}
	closeParams, ok := closeTargetCall.params.(proto.TargetCloseTarget)
	if !ok || closeParams.TargetID != "target-1" {
		t.Fatalf("Target.closeTarget params = %#v", closeTargetCall.params)
	}

	if client.calls[2].sessionID != "" {
		t.Fatalf("Browser.close session ID = %q, want empty", client.calls[2].sessionID)
	}
}

func TestNavigateWithCDPSurfacesNavigationError(t *testing.T) {
	response, err := json.Marshal(proto.PageNavigateResult{ErrorText: "blocked by policy"})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingCDPClient{response: response}

	err = navigateWithCDP(context.Background(), client, "session-1", "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Fatalf("navigateWithCDP error = %v", err)
	}
}

func TestObscuraEnvironmentTimeoutDefaults(t *testing.T) {
	for _, defaultValue := range obscuraTimeoutDefaults {
		key, _, _ := strings.Cut(defaultValue, "=")
		previous, configured := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if configured {
				_ = os.Setenv(key, previous)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}

	environment := obscuraEnvironment()
	for _, expected := range obscuraTimeoutDefaults {
		if !containsEnvironmentValue(environment, expected) {
			t.Errorf("Obscura environment does not contain %q", expected)
		}
	}
}

func TestObscuraEnvironmentPreservesOverride(t *testing.T) {
	t.Setenv("OBSCURA_NAV_TIMEOUT_MS", "12345")

	environment := obscuraEnvironment()
	if !containsEnvironmentValue(environment, "OBSCURA_NAV_TIMEOUT_MS=12345") {
		t.Fatal("Obscura environment did not preserve OBSCURA_NAV_TIMEOUT_MS override")
	}
	if containsEnvironmentValue(environment, "OBSCURA_NAV_TIMEOUT_MS=60000") {
		t.Fatal("Obscura environment appended a duplicate navigation timeout")
	}
}

func containsEnvironmentValue(environment []string, expected string) bool {
	for _, value := range environment {
		if value == expected {
			return true
		}
	}
	return false
}
