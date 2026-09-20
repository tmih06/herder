package main

import (
	"fmt"
	"io"
	"os"

	"github.com/tmih06/herder/internal/dispatch"
	"github.com/tmih06/herder/internal/storage"
)

// newDispatcher builds the shared dispatch engine for one CLI verb:
// info lines land on w, non-fatal warnings on ew, and the Docker
// provider logs through the same ew seam newProvider uses. The daemon
// wires its own dispatcher (owner "daemon-<pid>") instead of this one.
func newDispatcher(store *storage.Store, w, ew io.Writer) *dispatch.Dispatcher {
	return &dispatch.Dispatcher{
		Store:    store,
		Provider: newProvider(ew),
		Owner:    fmt.Sprintf("cli-%d", os.Getpid()),
		Logf:     func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) },
		Warnf:    func(format string, args ...any) { fmt.Fprintf(ew, format+"\n", args...) },
	}
}
