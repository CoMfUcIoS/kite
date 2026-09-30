package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// pagerCommand picks KITE_PAGER, then PAGER, then less. "" means don't page,
// and so does cat. A bare less gets -FRX: quit when the output fits one
// screen, keep colour, leave the output on screen. Flags add to $LESS rather
// than replace it, so a LESS=-R still applies.
func pagerCommand(lookup func(string) (string, bool)) string {
	p, ok := lookup("KITE_PAGER")
	if !ok {
		p, ok = lookup("PAGER")
	}
	if !ok {
		p = "less"
	}
	switch p = strings.TrimSpace(p); p {
	case "cat":
		return ""
	case "less":
		return "less -FRX"
	}
	return p
}

// page runs pager through sh, so it can carry arguments.
func page(out io.Writer, data []byte, pager string) error {
	cmd := exec.Command("sh", "-c", pager)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// emit writes finished output, through a pager when stdout is a terminal.
// Output is buffered first because the pager owns the terminal while it runs,
// and the spinner would draw over it. A pager that can't be found falls back
// to plain stdout; one that ran and exited non-zero already showed the output.
func emit(data []byte, usePager bool) {
	if usePager && isTerminal(os.Stdout) {
		if p := pagerCommand(os.LookupEnv); p != "" {
			err := page(os.Stdout, data, p)
			var ee *exec.ExitError
			if err == nil || errors.As(err, &ee) && ee.ExitCode() != 127 {
				return
			}
		}
	}
	os.Stdout.Write(data)
}
