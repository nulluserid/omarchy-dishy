package main

import (
	"os/exec"
)

// notifySend sends a desktop notification. Per CONTRACT.md a failure here is
// logged by the caller and never fatal. It is a variable so tests can stub it.
var notifySend = func(summary, body string) error {
	cmd := exec.Command("notify-send", "-a", "Dishy", "-u", "normal", summary, body)
	return cmd.Run()
}
