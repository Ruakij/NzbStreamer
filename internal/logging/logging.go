// Package logging puts the package a line came from in front of the line.
package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"runtime"
	"strings"
)

// Setup installs the handler every package logs through. The format is the one
// slog logs in by default - a date, a level, the message, then the attributes -
// with the module in front of the message, since what a line is about is read
// before what it says about it and an attribute is always printed behind.
//
// The handler writes to its own log.Logger rather than wrapping the default
// one: slog.SetDefault points the std log package at slog, so a handler that
// logged through it would feed itself.
func Setup(level slog.Level) {
	slog.SetDefault(slog.New(New(os.Stderr, level)))
}

// New is the handler Setup installs, writing to somewhere else.
func New(out io.Writer, level slog.Level) slog.Handler {
	return &handler{level: level, out: log.New(out, "", log.LstdFlags|log.Lmicroseconds)}
}

type handler struct {
	level slog.Level
	out   *log.Logger
	// attrs already rendered, from WithAttrs, and the group their keys are
	// prefixed with
	attrs string
	group string
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *handler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	line.WriteString(record.Level.String())

	if module := moduleOf(record.PC); module != "" {
		line.WriteString(" [" + module + "]")
	}

	line.WriteString(" " + record.Message)
	line.WriteString(h.attrs)
	record.Attrs(func(attr slog.Attr) bool {
		line.WriteString(render(h.group, attr))
		return true
	})

	return h.out.Output(0, line.String())
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	rendered := h.attrs
	for _, attr := range attrs {
		rendered += render(h.group, attr)
	}

	next := *h
	next.attrs = rendered
	return &next
}

func (h *handler) WithGroup(name string) slog.Handler {
	next := *h
	next.group = h.group + name + "."
	return &next
}

func render(group string, attr slog.Attr) string {
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		var rendered string
		for _, member := range value.Group() {
			rendered += render(group+attr.Key+".", member)
		}
		return rendered
	}

	text := value.String()
	if text == "" || strings.ContainsAny(text, " =\"") {
		text = fmt.Sprintf("%q", text)
	}
	return " " + group + attr.Key + "=" + text
}

// moduleOf is the package of the call that logged, which slog records as the
// program counter of the caller of Info and its siblings.
func moduleOf(pc uintptr) string {
	if pc == 0 {
		return ""
	}

	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()

	// git.ruekov.eu/ruakij/nzbStreamer/internal/service/nzbservice.(*Service).Init
	name := frame.Function
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if dot := strings.Index(name, "."); dot >= 0 {
		name = name[:dot]
	}
	return name
}
