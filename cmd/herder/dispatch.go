package main

import (
	"fmt"
	"io"

	"github.com/tmih06/herder/internal/dispatch"
	"github.com/tmih06/herder/internal/storage"
)

// newDispatcher builds the shared dispatch engine for one CLI verb:
// info lines land on w, non-fatal warnings — including the defaulted
// Docker provider's — on ew through Warnf. Owner and Provider stay
// nil-defaulted: the owner accessor already mints a per-process
// "cli-<pid>" and provider() wires Warnf the same way newProvider does.
// The daemon wires its own dispatcher (owner "daemon-<pid>") instead.
func newDispatcher(store *storage.Store, w, ew io.Writer) *dispatch.Dispatcher {
	return &dispatch.Dispatcher{
		Store: store,
		Logf:  func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) },
		Warnf: func(format string, args ...any) { fmt.Fprintf(ew, format+"\n", args...) },
	}
}
