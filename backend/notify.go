package main

import (
	"context"
	"os/exec"
	"time"
)

// notifySend sends a desktop notification. Per CONTRACT.md a failure here is
// logged by the caller and never fatal. It is a variable so tests can stub it.
// Context-bounded: a wedged notification daemon must not stall the tick loop.
// The "--" separator keeps summary/body out of notify-send's option parsing.
var notifySend = func(summary, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "notify-send", "-a", "Dishy", "-u", "normal", "--", summary, body)
	return cmd.Run()
}
