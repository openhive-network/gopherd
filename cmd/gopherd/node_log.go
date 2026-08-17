package main

import (
	"fmt"
	"io"
	"time"
)

// appLogger is a tiny stderr logger with millisecond timestamps. We don't
// pull in log/slog because (a) we need three lines of output and (b)
// keeping the daemon dependency-free is a project goal.
type appLogger struct {
	out io.Writer
}

func newAppLogger(out io.Writer) *appLogger { return &appLogger{out: out} }

func (l *appLogger) printf(format string, args ...interface{}) {
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	fmt.Fprintf(l.out, "[%s] gopherd: %s\n", stamp, fmt.Sprintf(format, args...))
}
