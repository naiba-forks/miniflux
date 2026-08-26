// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"errors"
	"testing"
)

func TestAIProcessingStateLifecycle(t *testing.T) {
	const userID = int64(987654321)
	aiProcessingStates.Delete(userID)
	activeAIProcessors.Delete(userID)
	t.Cleanup(func() {
		aiProcessingStates.Delete(userID)
		activeAIProcessors.Delete(userID)
	})

	if !tryStartAIProcessing(userID, AIProcessingModeBackfill, 4) {
		t.Fatal("expected to acquire the user's AI queue")
	}
	if tryStartAIProcessing(userID, AIProcessingModeAutomatic, 1) {
		t.Fatal("a second AI task must not acquire the same user's queue")
	}
	if !IsAIProcessingRunning(userID) {
		t.Fatal("expected the AI queue to be marked active")
	}
	setAIProcessingCurrentEntry(userID, 42, "Example entry")
	recordAIProcessingSuccess(userID)
	recordAIProcessingFailure(userID, errors.New("provider unavailable"))
	finishAIProcessing(userID, AIProcessingStatusCompletedWithErrors, nil)
	if IsAIProcessingRunning(userID) {
		t.Fatal("expected the AI queue to be released")
	}

	state, ok := GetAIProcessingState(userID)
	if !ok {
		t.Fatal("expected an AI processing state")
	}
	if state.Status != AIProcessingStatusCompletedWithErrors {
		t.Fatalf("unexpected status: %q", state.Status)
	}
	if state.Mode != AIProcessingModeBackfill || state.Total != 4 {
		t.Fatalf("unexpected task metadata: mode=%q total=%d", state.Mode, state.Total)
	}
	if state.Processed != 1 || state.Failed != 1 {
		t.Fatalf("unexpected counters: processed=%d failed=%d", state.Processed, state.Failed)
	}
	if state.LastError != "provider unavailable" || state.LastErrorAt == nil {
		t.Fatalf("unexpected error state: error=%q at=%v", state.LastError, state.LastErrorAt)
	}
	if state.LastSuccessAt == nil || state.FinishedAt == nil {
		t.Fatal("expected success and finish timestamps")
	}
	if state.CurrentEntryID != 0 || state.CurrentEntryTitle != "" {
		t.Fatal("finished state must not retain a current entry")
	}
}

func TestStopBackfillUpdatesVisibleState(t *testing.T) {
	const userID = int64(987654322)
	aiProcessingStates.Delete(userID)
	backfillStopSignals.Delete(userID)
	activeBackfills.Store(userID, true)
	t.Cleanup(func() {
		aiProcessingStates.Delete(userID)
		backfillStopSignals.Delete(userID)
		activeBackfills.Delete(userID)
	})

	startAIProcessing(userID, AIProcessingModeBackfill, 1)
	StopBackfill(userID)

	state, ok := GetAIProcessingState(userID)
	if !ok || state.Status != AIProcessingStatusStopping {
		t.Fatalf("expected stopping state, got %#v", state)
	}
	if !isBackfillStopped(userID) {
		t.Fatal("expected a stop signal")
	}
}
