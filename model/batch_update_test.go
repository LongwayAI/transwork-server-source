package model

import (
	"testing"
	"time"
)

// The periodic updater detaches a store under its per-type lock and then writes
// to the database with that lock released. A shutdown flush that only inspected
// the stores would therefore find them empty mid-tick, report itself done, and
// let main's deferred CloseDB tear the connection pool down underneath writes
// still in flight — losing the very billing records the flush exists to save.
//
// So the contract is not "the maps are empty" but "no batchUpdate is still
// running": FlushBatchUpdates must block until an in-flight tick completes.
func TestFlushBatchUpdatesWaitsForInFlightUpdater(t *testing.T) {
	// Stand in for a periodic tick that has detached its store and is partway
	// through writing it out.
	batchUpdateRunning.Lock()
	unlocked := false
	release := func() {
		if !unlocked {
			unlocked = true
			batchUpdateRunning.Unlock()
		}
	}
	defer release()

	done := make(chan struct{})
	go func() {
		FlushBatchUpdates()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("FlushBatchUpdates returned while another batchUpdate was still running; " +
			"its database writes would be abandoned when main closes the pool")
	case <-time.After(100 * time.Millisecond):
		// Still blocked, which is the required behavior.
	}

	release()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FlushBatchUpdates never returned after the in-flight updater finished")
	}
}
