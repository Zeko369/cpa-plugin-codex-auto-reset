package main

import "strings"

func resetProvider(provider, kind string) bool {
	return strings.EqualFold(provider, "codex") || strings.EqualFold(kind, "codex") || strings.EqualFold(provider, "claude") || strings.EqualFold(kind, "claude")
}

func stopCurrentWorker() {
	pluginStateMu.Lock()
	w := globalWorker
	pluginStateMu.Unlock()
	if w == nil {
		return
	}
	w.Stop()
	w.mu.Lock()
	running := w.running
	w.mu.Unlock()
	if running {
		<-w.done
	}
}

func wireDurability(w *Worker) {
	w.Restore = func(id string) (AccountRuntime, bool) {
		pluginStateMu.Lock()
		defer pluginStateMu.Unlock()
		r, ok := globalState.Accounts[id]
		return r, ok
	}
	w.Persist = func(r AccountRuntime) error {
		pluginStateMu.Lock()
		defer pluginStateMu.Unlock()
		globalState.Accounts[r.AuthID] = r
		return saveState(globalStatePath, globalState)
	}
}
