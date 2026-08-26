// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration // import "miniflux.app/v2/internal/integration"

import (
	"sync"
	"time"
)

const (
	AIProcessingStatusRunning             = "running"
	AIProcessingStatusStopping            = "stopping"
	AIProcessingStatusCompleted           = "completed"
	AIProcessingStatusCompletedWithErrors = "completed_with_errors"
	AIProcessingStatusFailed              = "failed"
	AIProcessingStatusStopped             = "stopped"

	AIProcessingModeAutomatic     = "automatic"
	AIProcessingModeBackfill      = "backfill"
	AIProcessingModeForceBackfill = "force_backfill"
)

// AIProcessingState is an ephemeral snapshot of AI summary processing for one
// user. It intentionally lives in memory: a process restart clears old status
// and errors instead of adding operational data to the database.
type AIProcessingState struct {
	Status            string     `json:"status"`
	Mode              string     `json:"mode"`
	Total             int        `json:"total"`
	Processed         int        `json:"processed"`
	Failed            int        `json:"failed"`
	CurrentEntryID    int64      `json:"current_entry_id,omitempty"`
	CurrentEntryTitle string     `json:"current_entry_title,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	LastSuccessAt     *time.Time `json:"last_success_at,omitempty"`
	LastErrorAt       *time.Time `json:"last_error_at,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
}

type aiProcessingStateValue struct {
	mu    sync.RWMutex
	state AIProcessingState
}

var (
	aiProcessingStates sync.Map
	activeAIProcessors sync.Map
)

func tryStartAIProcessing(userID int64, mode string, total int) bool {
	if _, alreadyRunning := activeAIProcessors.LoadOrStore(userID, true); alreadyRunning {
		return false
	}
	startAIProcessing(userID, mode, total)
	return true
}

func startAIProcessing(userID int64, mode string, total int) {
	aiProcessingStates.Store(userID, &aiProcessingStateValue{
		state: AIProcessingState{
			Status:    AIProcessingStatusRunning,
			Mode:      mode,
			Total:     total,
			StartedAt: time.Now(),
		},
	})
}

func setAIProcessingCurrentEntry(userID, entryID int64, title string) {
	if state, ok := loadAIProcessingState(userID); ok {
		state.mu.Lock()
		state.state.CurrentEntryID = entryID
		state.state.CurrentEntryTitle = title
		state.mu.Unlock()
	}
}

func setAIProcessingTotal(userID int64, total int) {
	if state, ok := loadAIProcessingState(userID); ok {
		state.mu.Lock()
		state.state.Total = total
		state.mu.Unlock()
	}
}

func recordAIProcessingSuccess(userID int64) {
	if state, ok := loadAIProcessingState(userID); ok {
		now := time.Now()
		state.mu.Lock()
		state.state.Processed++
		state.state.LastSuccessAt = &now
		state.mu.Unlock()
	}
}

func recordAIProcessingFailure(userID int64, err error) {
	if state, ok := loadAIProcessingState(userID); ok {
		now := time.Now()
		state.mu.Lock()
		state.state.Failed++
		state.state.LastError = err.Error()
		state.state.LastErrorAt = &now
		state.mu.Unlock()
	}
}

func finishAIProcessing(userID int64, status string, err error) {
	defer activeAIProcessors.Delete(userID)
	if state, ok := loadAIProcessingState(userID); ok {
		now := time.Now()
		state.mu.Lock()
		state.state.Status = status
		state.state.CurrentEntryID = 0
		state.state.CurrentEntryTitle = ""
		state.state.FinishedAt = &now
		if err != nil {
			state.state.LastError = err.Error()
			state.state.LastErrorAt = &now
		}
		state.mu.Unlock()
	}
}

func markAIProcessingStopping(userID int64) {
	if state, ok := loadAIProcessingState(userID); ok {
		state.mu.Lock()
		if state.state.Status == AIProcessingStatusRunning {
			state.state.Status = AIProcessingStatusStopping
		}
		state.mu.Unlock()
	}
}

func loadAIProcessingState(userID int64) (*aiProcessingStateValue, bool) {
	value, ok := aiProcessingStates.Load(userID)
	if !ok {
		return nil, false
	}
	return value.(*aiProcessingStateValue), true
}

// GetAIProcessingState returns a race-free copy of the latest in-memory state.
func GetAIProcessingState(userID int64) (AIProcessingState, bool) {
	state, ok := loadAIProcessingState(userID)
	if !ok {
		return AIProcessingState{}, false
	}

	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.state, true
}

// IsAIProcessingRunning reports whether any in-memory AI task currently owns
// the user's queue, including the automatic worker.
func IsAIProcessingRunning(userID int64) bool {
	_, running := activeAIProcessors.Load(userID)
	return running
}
