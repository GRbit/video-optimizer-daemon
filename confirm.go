package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// confirmFunc asks before an irreversible step and reports whether to go on.
// Two adapters exist: terminalConfirm for -prompt runs and alwaysConfirm for
// the daemon. The converter never learns which one it got.
type confirmFunc func(ctx context.Context, question string) (bool, error)

func alwaysConfirm(context.Context, string) (bool, error) {
	return true, nil
}

// terminalConfirm prints the question and reads one line from stdin. Reading
// happens in a goroutine so a shutdown signal is not stuck behind the prompt.
func terminalConfirm(ctx context.Context, question string) (bool, error) {
	fmt.Print(question)

	var (
		response string
		errCh    = make(chan error, 1)
	)
	go func() {
		var err error
		response, err = bufio.NewReader(os.Stdin).ReadString('\n')
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case err := <-errCh:
		if err != nil {
			return false, fmt.Errorf("reading user input: %w", err)
		}
	}

	response = strings.TrimSpace(strings.ToLower(response))
	slog.Debug("Prompt answered", "response", response)
	return response == "y" || response == "yes", nil
}
