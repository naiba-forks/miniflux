// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"net/http"

	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/ui/view"
)

func (h *handler) showAIDigestPage(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.UserByID(request.UserID(r))
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	offset := request.QueryIntParam(r, "offset", 0)

	countBuilder := h.store.NewEntryQueryBuilder(user.ID)
	countBuilder.WithStatuses(model.EntryStatusUnread)
	countBuilder.WithMinAIScore(1)
	countBuilder.WithGloballyVisible()
	total, err := countBuilder.CountEntries()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	if offset >= total {
		offset = 0
	}

	builder := h.store.NewEntryQueryBuilder(user.ID)
	builder.WithStatuses(model.EntryStatusUnread)
	builder.WithMinAIScore(1)
	builder.WithGloballyVisible()
	builder.WithSorting("ai_score", "DESC")
	builder.WithSorting("id", "DESC")
	builder.WithOffset(offset)
	builder.WithLimit(user.EntriesPerPage)

	entries, err := builder.GetEntries()
	if err != nil {
		response.HTMLServerError(w, r, err)
		return
	}

	view := view.New(h.tpl, r)
	view.Set("entries", entries)
	view.Set("total", total)
	view.Set("pagination", getPagination(h.routePath("/ai-digest"), total, offset, user.EntriesPerPage))
	view.Set("menu", "ai-digest")
	view.Set("user", user)
	navMetadata, _ := h.store.GetNavMetadata(user.ID)
	view.Set("countUnread", navMetadata.CountUnread)
	view.Set("countErrorFeeds", navMetadata.CountErrorFeeds)
	view.Set("showAIDigest", navMetadata.ShowAIDigest)
	view.Set("countAIDigest", total)
	view.Set("hasSaveEntry", navMetadata.HasSaveEntry)

	response.HTML(w, r, view.Render("ai_digest"))
}
