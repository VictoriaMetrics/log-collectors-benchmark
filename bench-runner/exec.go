package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

func sh(ctx context.Context, name string, args ...string) {
	command := fmt.Sprintf("%s %s", name, strings.Join(args, " "))
	slog.Info("running", slog.String("command", command))

	cmd := exec.CommandContext(ctx, name, args...)

	wrappedStderr := newLinePrefixWriter(os.Stderr, name)
	defer wrappedStderr.Close()
	wrappedStdout := newLinePrefixWriter(os.Stdout, name)
	defer wrappedStdout.Close()

	cmd.Stderr = wrappedStderr
	cmd.Stdout = wrappedStdout

	if err := cmd.Run(); err != nil {
		panic(fmt.Errorf("failed to run command %q: %w", command, err))
	}
}

// linePrefixWriter implements io.Writer that wraps each written line with given prefix.
type linePrefixWriter struct {
	out    io.Writer
	prefix string
	buf    []byte
}

func newLinePrefixWriter(out io.Writer, prefix string) *linePrefixWriter {
	w := &linePrefixWriter{
		out:    out,
		prefix: prefix,
	}
	w.resetBufWithPrefix()
	return w
}

func (w *linePrefixWriter) Write(data []byte) (int, error) {
	written := len(data)
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n')
		if n == -1 {
			// Line is not ended.
			if len(w.buf) > 10*1024*1024 {
				panic(fmt.Errorf("BUG: line too long; line=%q", w.buf))
			}
			w.buf = append(w.buf, data...)
			break
		}
		line := data[:n]
		data = data[n+1:]

		w.buf = append(w.buf, line...)
		w.buf = append(w.buf, '\n')
		n, err := w.out.Write(w.buf)
		if err != nil {
			return 0, err
		}
		if n != len(w.buf) {
			return 0, fmt.Errorf("%d bytes written out of %d bytes", n, len(w.buf))
		}
		w.resetBufWithPrefix()
	}
	return written, nil
}

func (w *linePrefixWriter) Close() error {
	// Handle incomplete lines.
	tail := bytes.Clone(w.buf)
	w.resetBufWithPrefix()
	prefix := w.buf
	if !bytes.Equal(tail, prefix) {
		_, err := w.Write(tail)
		return err
	}
	return nil
}

func (w *linePrefixWriter) resetBufWithPrefix() {
	// Set green color to the prefix to improve readability of logs produced by external programs.
	// See available ANSII codes here: https://gist.github.com/fnky/458719343aabd01cfb17a3a4f7296797#color-codes
	const green = "\033[32m"
	const reset = "\033[0m"

	w.buf = w.buf[:0]
	w.buf = append(w.buf, green...)
	w.buf = append(w.buf, w.prefix...)
	w.buf = append(w.buf, reset...)
	w.buf = append(w.buf, ": "...)
}
