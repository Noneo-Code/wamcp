package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// stderrLogger is a waLog.Logger that never writes to stdout, which carries
// the MCP protocol in stdio mode.
type stderrLogger struct {
	out    io.Writer
	mu     *sync.Mutex
	module string
	min    int
}

var levels = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

func newLogger(out io.Writer, level string) (waLog.Logger, error) {
	lvl, ok := levels[strings.ToLower(level)]
	if !ok {
		return nil, fmt.Errorf("unknown log level %q (debug, info, warn, error)", level)
	}
	return &stderrLogger{out: out, mu: &sync.Mutex{}, min: lvl}, nil
}

func (l *stderrLogger) log(lvl int, name, msg string, args ...any) {
	if lvl < l.min {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := name
	if l.module != "" {
		prefix += " [" + l.module + "]"
	}
	fmt.Fprintf(l.out, "%s %s %s\n", time.Now().Format("15:04:05"), prefix, fmt.Sprintf(msg, args...))
}

func (l *stderrLogger) Debugf(msg string, args ...any) { l.log(0, "DEBUG", msg, args...) }
func (l *stderrLogger) Infof(msg string, args ...any)  { l.log(1, "INFO ", msg, args...) }
func (l *stderrLogger) Warnf(msg string, args ...any)  { l.log(2, "WARN ", msg, args...) }
func (l *stderrLogger) Errorf(msg string, args ...any) { l.log(3, "ERROR", msg, args...) }

func (l *stderrLogger) Sub(module string) waLog.Logger {
	sub := *l
	if l.module != "" {
		module = l.module + "/" + module
	}
	sub.module = module
	return &sub
}
