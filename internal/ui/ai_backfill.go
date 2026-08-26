// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	json_parser "encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/integration"
	"miniflux.app/v2/internal/integration/ai"
	"miniflux.app/v2/internal/model"
)

func (h *handler) aiBackfill(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)

	if integration.IsAIProcessingRunning(userID) {
		response.NoContent(w, r)
		return
	}

	userIntegrations, err := h.store.Integration(userID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	user, err := h.store.UserByID(userID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	go integration.BackfillAISummaries(h.store, userID, userIntegrations, user.Language)

	response.NoContent(w, r)
}

func (h *handler) aiForceBackfill(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)

	if integration.IsAIProcessingRunning(userID) {
		response.NoContent(w, r)
		return
	}

	userIntegrations, err := h.store.Integration(userID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	user, err := h.store.UserByID(userID)
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	go integration.ForceBackfillAISummaries(h.store, userID, userIntegrations, user.Language)

	response.NoContent(w, r)
}

type aiBackfillStatusResponse struct {
	Running     bool                           `json:"running"`
	Stoppable   bool                           `json:"stoppable"`
	Pending     int                            `json:"pending"`
	Summarized  int                            `json:"summarized"`
	Failed      int                            `json:"failed"`
	LastError   string                         `json:"last_error,omitempty"`
	LastErrorAt *time.Time                     `json:"last_error_at,omitempty"`
	MaxAttempts int                            `json:"max_attempts"`
	State       *integration.AIProcessingState `json:"state,omitempty"`
}

func (h *handler) aiBackfillStatus(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)

	stats, err := h.store.AISummaryQueueStats(userID)
	if err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	backfillRunning := integration.IsBackfillRunning(userID)
	result := aiBackfillStatusResponse{
		Running:     integration.IsAIProcessingRunning(userID) || backfillRunning,
		Stoppable:   backfillRunning,
		Pending:     stats.Pending,
		Summarized:  stats.Summarized,
		Failed:      stats.Failed,
		LastError:   stats.LastError,
		LastErrorAt: stats.LastErrorAt,
		MaxAttempts: model.MaxAISummaryFailures,
	}
	if state, ok := integration.GetAIProcessingState(userID); ok {
		result.State = &state
	}

	response.JSON(w, r, result)
}

func (h *handler) aiStopBackfill(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)
	integration.StopBackfill(userID)
	response.NoContent(w, r)
}

type aiTestConnectionRequest struct {
	ProviderURL string `json:"provider_url"`
	APIKey      string `json:"api_key"`
	Model       string `json:"model"`
}

func (h *handler) aiTestConnection(w http.ResponseWriter, r *http.Request) {
	var req aiTestConnectionRequest
	if err := json_parser.NewDecoder(r.Body).Decode(&req); err != nil {
		response.JSONBadRequest(w, r, err)
		return
	}

	req.ProviderURL = strings.TrimSpace(req.ProviderURL)
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.Model = strings.TrimSpace(req.Model)
	if req.ProviderURL == "" || req.APIKey == "" || req.Model == "" {
		response.JSONBadRequest(w, r, errors.New("provider URL, API key, and model are required"))
		return
	}

	if err := ai.NewClient(req.ProviderURL, req.APIKey, req.Model).TestConnection(); err != nil {
		response.JSONBadRequest(w, r, err)
		return
	}

	response.JSON(w, r, map[string]bool{"ok": true})
}

type pageSummaryResult struct {
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"`
}

var activePageSummaries sync.Map

type aiPageSummaryRequest struct {
	EntryIDs []int64 `json:"entry_ids"`
}

func (h *handler) aiPageSummary(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)

	var req aiPageSummaryRequest
	if err := json_parser.NewDecoder(r.Body).Decode(&req); err != nil {
		response.JSONBadRequest(w, r, err)
		return
	}

	if len(req.EntryIDs) == 0 {
		response.JSONBadRequest(w, r, errors.New("entry_ids is required"))
		return
	}

	userIntegrations, err := h.store.Integration(userID)
	if err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	if !userIntegrations.AIEnabled || userIntegrations.AIProviderURL == "" || userIntegrations.AIAPIKey == "" || userIntegrations.AIModel == "" {
		response.JSONBadRequest(w, r, errors.New("AI integration is not configured"))
		return
	}

	user, err := h.store.UserByID(userID)
	if err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	var summaryParts []string
	for _, entryID := range req.EntryIDs {
		builder := h.store.NewEntryQueryBuilder(userID)
		builder.WithEntryIDs(entryID)
		entry, entryErr := builder.GetEntry()
		if entryErr != nil || entry == nil {
			continue
		}
		if entry.AISummary != "" {
			feedTitle := ""
			if entry.Feed != nil {
				feedTitle = entry.Feed.Title
			}
			summaryParts = append(summaryParts, fmt.Sprintf("[Source: %s] %s: %s", feedTitle, entry.Title, entry.AISummary))
		}
	}

	if len(summaryParts) == 0 {
		response.JSONBadRequest(w, r, errors.New("no entries with AI summaries found"))
		return
	}

	combinedInput := ""
	for _, part := range summaryParts {
		combinedInput += part + "\n\n"
	}

	activePageSummaries.Store(userID, &pageSummaryResult{Status: "running"})

	go func() {
		client := ai.NewClient(
			userIntegrations.AIProviderURL,
			userIntegrations.AIAPIKey,
			userIntegrations.AIModel,
		)

		result, err := client.GeneratePageSummary(combinedInput, user.Language)
		if err != nil {
			activePageSummaries.Store(userID, &pageSummaryResult{
				Status: "error",
				Error:  err.Error(),
			})
			return
		}

		activePageSummaries.Store(userID, &pageSummaryResult{
			Status:  "done",
			Summary: result,
		})
	}()

	response.JSONAccepted(w, r)
}

func (h *handler) aiPageSummaryStatus(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)

	val, ok := activePageSummaries.Load(userID)
	if !ok {
		response.JSON(w, r, &pageSummaryResult{Status: "idle"})
		return
	}

	result := val.(*pageSummaryResult)
	response.JSON(w, r, result)

	if result.Status == "done" || result.Status == "error" {
		activePageSummaries.Delete(userID)
	}
}
