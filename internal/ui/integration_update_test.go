// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/ui/form"
)

func TestAIConfigurationChanged(t *testing.T) {
	current := &model.Integration{
		AIEnabled:     true,
		AIProviderURL: "https://example.test/v1",
		AIAPIKey:      "secret",
		AIModel:       "model-a",
	}

	tests := []struct {
		name    string
		form    form.IntegrationForm
		changed bool
	}{
		{
			name: "unchanged",
			form: form.IntegrationForm{
				AIEnabled:     true,
				AIProviderURL: "https://example.test/v1",
				AIAPIKey:      "secret",
				AIModel:       "model-a",
			},
		},
		{
			name: "enabled changed",
			form: form.IntegrationForm{
				AIProviderURL: "https://example.test/v1",
				AIAPIKey:      "secret",
				AIModel:       "model-a",
			},
			changed: true,
		},
		{
			name: "provider changed",
			form: form.IntegrationForm{
				AIEnabled:     true,
				AIProviderURL: "https://other.test/v1",
				AIAPIKey:      "secret",
				AIModel:       "model-a",
			},
			changed: true,
		},
		{
			name: "API key changed",
			form: form.IntegrationForm{
				AIEnabled:     true,
				AIProviderURL: "https://example.test/v1",
				AIAPIKey:      "new-secret",
				AIModel:       "model-a",
			},
			changed: true,
		},
		{
			name: "model changed",
			form: form.IntegrationForm{
				AIEnabled:     true,
				AIProviderURL: "https://example.test/v1",
				AIAPIKey:      "secret",
				AIModel:       "model-b",
			},
			changed: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := aiConfigurationChanged(current, &test.form); got != test.changed {
				t.Fatalf("unexpected result: got %v, want %v", got, test.changed)
			}
		})
	}
}
