package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

// copyFile is the cross-device fallback for os.Rename. The original is deleted
// right after it succeeds, so a partial destination is removed on any failure
// and the copy is fsynced and size-checked before reporting success.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening input file: %w", err)
	}
	defer closeCloser(in)

	srcStat, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat input file: %w", err)
	}

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer func() {
		if err != nil {
			if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
				slog.Warn("Failed to remove partial copy", "path", dst, "err", rmErr)
			}
		}
	}()

	written, err := io.Copy(out, in)
	if err != nil {
		closeCloser(out)
		return fmt.Errorf("copying data: %w", err)
	}
	if err = out.Sync(); err != nil {
		closeCloser(out)
		return fmt.Errorf("syncing output file: %w", err)
	}
	if err = out.Close(); err != nil {
		return fmt.Errorf("closing output file: %w", err)
	}

	if written != srcStat.Size() {
		err = fmt.Errorf("size mismatch after copy: source %d bytes, written %d bytes", srcStat.Size(), written)
		return err
	}

	return nil
}

// fileSize returns 0 when the file cannot be stat'ed; callers treat 0 as
// "unknown" and never as a real size.
func fileSize(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		slog.Warn("Failed to stat file", "path", p, "err", err)
		return 0
	}
	return info.Size()
}

func closeCloser(c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Warn("Failed to close", "err", err)
	}
}
