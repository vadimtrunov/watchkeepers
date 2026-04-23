package publish

// WaitWatchdogs blocks until every per-subscription watchdog goroutine
// spawned by Subscribe has exited. Intended for unit tests that assert
// Close() releases all watchdogs without calling cancel on the per-
// subscription ctx. Not exported in the public API because production
// callers have no reason to synchronise on a background bookkeeping
// goroutine.
func (r *Registry) WaitWatchdogs() {
	r.watchdogs.Wait()
}
