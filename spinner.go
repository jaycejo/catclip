package catclip

import (
	"fmt"
	"os"
	"time"
)

// startLoadingSpinner shows a small TTY-only spinner for slow interactive
// setup work such as building picker candidate lists. It clears itself before
// returning control to fzf or normal stderr output.
func startLoadingSpinner(output *os.File, message string) func() {
	if output == nil || !isTerminalFile(output) {
		return func() {}
	}
	if os.Getenv("TERM") == "dumb" {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	frames := []string{"|", "/", "-", `\`}
	drawFrame := func(frame string) {
		_, _ = fmt.Fprintf(output, "\r\033[K%s %s", frame, message)
	}

	// Draw immediately so short-lived stages still show a visible loading label.
	drawFrame(frames[0])
	go func() {
		defer close(done)
		ticker := time.NewTicker(90 * time.Millisecond)
		defer ticker.Stop()

		i := 1
		for {
			select {
			case <-stop:
				_, _ = fmt.Fprint(output, "\r\033[K")
				return
			case <-ticker.C:
				drawFrame(frames[i%len(frames)])
				i++
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

func spinnerOutputFile(w any) *os.File {
	if file, ok := w.(*os.File); ok {
		return file
	}
	return nil
}

func outputSpinnerMessage(cfg runConfig) string {
	if cfg.OutputMode == outputModeClipboard {
		return "Copying files..."
	}
	return "Writing files..."
}
