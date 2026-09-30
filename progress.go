package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var spinFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// progress draws a one-line spinner on stderr while kite works. A nil
// *progress does nothing, so callers never check whether it's on.
type progress struct {
	w     io.Writer
	mu    sync.Mutex
	label string
	total int64
	done  atomic.Int64
	quit  chan struct{}
	wg    sync.WaitGroup
}

var prog *progress

// startProgress returns nil unless stderr is a terminal, so pipes, CI logs
// and agents never see control codes.
func startProgress() *progress {
	if !isTerminal(os.Stderr) || os.Getenv("TERM") == "dumb" {
		return nil
	}
	p := &progress{w: os.Stderr, quit: make(chan struct{})}
	p.wg.Add(1)
	go p.loop()
	return p
}

func (p *progress) loop() {
	defer p.wg.Done()
	// The delay keeps a fast run from flashing a spinner.
	select {
	case <-p.quit:
		return
	case <-time.After(150 * time.Millisecond):
	}
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		p.render(i)
		select {
		case <-p.quit:
			return
		case <-t.C:
		}
	}
}

func (p *progress) phase(label string, total int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.label, p.total = label, int64(total)
	p.done.Store(0)
	p.mu.Unlock()
}

// tick is safe from the fan goroutines.
func (p *progress) tick() {
	if p != nil {
		p.done.Add(1)
	}
}

func (p *progress) render(frame int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.label == "" {
		return
	}
	fmt.Fprintf(p.w, "\r\033[K%c %s %d/%d", spinFrames[frame%len(spinFrames)], p.label, p.done.Load(), p.total)
}

// stop clears the line. Call it before anything else is printed.
func (p *progress) stop() {
	if p == nil {
		return
	}
	if p.quit != nil {
		close(p.quit)
		p.wg.Wait()
	}
	p.mu.Lock()
	fmt.Fprint(p.w, "\r\033[K")
	p.mu.Unlock()
}

func fmtDur(d time.Duration) string {
	s := d.Seconds()
	switch {
	case s < 1:
		return fmt.Sprintf("%.2fs", s)
	case s < 10:
		return fmt.Sprintf("%.1fs", s)
	}
	return fmt.Sprintf("%.0fs", s)
}

// timingLine is the footer's last part. GitHub is left out when kite never
// asked it anything.
func timingLine(total, local, gh time.Duration) string {
	parts := []string{"took " + fmtDur(total)}
	if gh > 0 {
		parts = append(parts, "git "+fmtDur(local), "GitHub "+fmtDur(gh))
	}
	return strings.Join(parts, " · ")
}
