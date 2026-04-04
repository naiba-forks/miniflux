// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	json_parser "encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/integration"
	"miniflux.app/v2/internal/integration/ai"
)

func (h *handler) aiBackfill(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)

	if integration.IsBackfillRunning(userID) {
		response.HTMLRedirect(w, r, h.routePath("/integrations"))
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

	response.HTMLRedirect(w, r, h.routePath("/integrations"))
}

func (h *handler) aiForceBackfill(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)

	if integration.IsBackfillRunning(userID) {
		response.HTMLRedirect(w, r, h.routePath("/integrations"))
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

	response.HTMLRedirect(w, r, h.routePath("/integrations"))
}

func (h *handler) aiBackfillStatus(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)
	response.JSON(w, r, map[string]bool{"running": integration.IsBackfillRunning(userID)})
}

func (h *handler) aiStopBackfill(w http.ResponseWriter, r *http.Request) {
	userID := request.UserID(r)
	integration.StopBackfill(userID)
	response.NoContent(w, r)
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
		builder.WithEntryID(entryID)
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
