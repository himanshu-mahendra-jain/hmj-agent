package terminal

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"hmj-agent/internal/terminal/theme"
)

// StartAccessibleProgress prints an append-only "[running] label (Ns)" line every
// accessibility report interval while a long operation runs. It does nothing outside
// accessibility mode. The returned stop function is idempotent: it ends the ticker and waits
// for its goroutine to exit, so no line is written after stop returns.
func StartAccessibleProgress(label string) (stop func()) {
	if !theme.Accessibility() {
		return func() {}
	}
	return startProgress(os.Stdout, label, accessibleReportInterval)
}

// startProgress runs the ticker goroutine that writes the progress lines to w.
func startProgress(w io.Writer, label string, every time.Duration) (stop func()) {
	started := time.Now()
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				elapsed := int(time.Since(started).Truncate(time.Second).Seconds())
				fmt.Fprintf(w, "[running] %s (%ds)\n", theme.ASCII(label), elapsed)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}
