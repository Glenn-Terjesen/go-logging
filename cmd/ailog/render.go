package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxCompactLine = 4096
	maxSummaryLine = 400
	maxEntry       = maxSummaryLine - 20 // room for the summary's indent and labels
	clipReserve    = 32                  // room for " …[truncated, N chars]"
	stackFrames    = 2
	stackGroups    = 2
)

var severityLabels = map[string]string{
	"trace": "TRC", "debug": "DBG", "info": "INF", "warning": "WRN", "warn": "WRN",
	"error": "ERR", "fatal": "FTL", "panic": "PNC",
}

// logEvent is a logical line recognised as a zerolog JSON event.
type logEvent struct {
	fields         map[string]json.RawMessage
	sevKey, msgKey string
	label          string
	key            [sha256.Size]byte // identity for dedupe: every field except the timestamp
	repeated       bool              // same as the previous event of its stream and hint
}

func parseLogEvent(text []byte) *logEvent {
	trimmed := bytes.TrimSpace(text)
	if !bytes.HasPrefix(trimmed, []byte("{")) {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(trimmed, &fields) != nil {
		return nil
	}
	ev := &logEvent{fields: fields, sevKey: firstKey(fields, "severity", "level"), msgKey: firstKey(fields, "message", "msg")}
	var sev string
	if ev.sevKey == "" || ev.msgKey == "" || json.Unmarshal(fields[ev.sevKey], &sev) != nil {
		return nil
	}
	ev.label = severityLabels[strings.ToLower(sev)]
	if ev.label == "" {
		ev.label = escapeText(strings.ToUpper(sev))
	}

	var id []byte
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		if k != "timestamp" && k != "time" {
			id = strconv.AppendQuote(id, k)
			id = append(id, ':')
			id = append(id, fields[k]...)
			id = append(id, ',')
		}
	}
	ev.key = sha256.Sum256(id)
	return ev
}

func (e *logEvent) severe() bool {
	switch e.label {
	case "WRN", "ERR", "FTL", "PNC":
		return true
	default:
		return false
	}
}

// render returns the compact form without the leading "#N ".
func (e *logEvent) render(pkg, hint, cwd string) string {
	var b strings.Builder
	b.WriteString(e.label + " " + pkg + " ?" + hint)
	if raw, ok := e.fields["caller"]; ok {
		var caller string
		if json.Unmarshal(raw, &caller) == nil {
			b.WriteString(" at=" + bare(trimCwd(caller, cwd)))
		} else {
			b.WriteString(" at=" + jsonValue(raw))
		}
	}
	b.WriteString(" msg=" + jsonValue(e.fields[e.msgKey]))
	if raw, ok := e.fields["error"]; ok {
		b.WriteString(" error=" + jsonValue(raw))
	}
	if raw, ok := e.fields["stack"]; ok {
		if stack, ok := renderStack(raw, cwd); ok {
			b.WriteString(" stack=" + strconv.Quote(stack))
		} else {
			b.WriteString(" stack=" + jsonValue(raw))
		}
	}

	shown := []string{e.sevKey, e.msgKey, "timestamp", "time", "caller", "error", "stack"}
	for _, k := range slices.Sorted(maps.Keys(e.fields)) {
		if !slices.Contains(shown, k) {
			b.WriteString(" " + bare(k) + "=" + jsonValue(e.fields[k]))
		}
	}
	return b.String()
}

type frame struct {
	File     string          `json:"file"`
	Function string          `json:"function"`
	Line     json.RawMessage `json:"line"`
}

// renderStack renders go-logging's [][]{file,function,line} stack shape.
func renderStack(raw json.RawMessage, cwd string) (string, bool) {
	var groups [][]frame
	if json.Unmarshal(raw, &groups) != nil || len(groups) == 0 {
		return "", false
	}
	parts := make([]string, 0, stackGroups)
	for _, group := range groups[:min(stackGroups, len(groups))] {
		frames := make([]string, 0, stackFrames)
		for _, f := range group[:min(stackFrames, len(group))] {
			if f.File == "" && f.Function == "" {
				return "", false
			}
			line := strings.Trim(string(f.Line), `"`)
			frames = append(frames, f.Function+" "+trimCwd(f.File, cwd)+":"+line)
		}
		part := strings.Join(frames, " > ")
		if len(group) > stackFrames {
			part += fmt.Sprintf(" [+%d frames]", len(group)-stackFrames)
		}
		parts = append(parts, part)
	}
	out := strings.Join(parts, " | ")
	if len(groups) > stackGroups {
		out += fmt.Sprintf(" [+%d groups]", len(groups)-stackGroups)
	}
	return out, true
}

func firstKey(fields map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if _, ok := fields[k]; ok {
			return k
		}
	}
	return ""
}

// jsonValue renders strings quoted and everything else as minified JSON, keeping numbers exact.
func jsonValue(raw json.RawMessage) string {
	var s string
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) && json.Unmarshal(raw, &s) == nil {
		return strconv.Quote(s)
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return strconv.Quote(string(raw))
	}
	return escapeText(buf.String())
}

// trimCwd strips the working directory from a path, at a separator boundary only.
func trimCwd(path, cwd string) string {
	if cwd != "" && len(path) > len(cwd) && strings.HasPrefix(path, cwd) {
		return path[len(cwd):]
	}
	return path
}

// bare returns s unquoted when that is unambiguous in a k=v line.
func bare(s string) string {
	if s == "" || !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool {
		return r == ' ' || r == '"' || r == '=' || !unicode.IsPrint(r)
	}) {
		return strconv.Quote(s)
	}
	return s
}

// escapeText escapes control characters, other non-printable runes and invalid UTF-8.
func escapeText(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, needsEscape) {
		return s
	}
	var b strings.Builder
	for s != "" {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 || needsEscape(r) {
			q := strconv.Quote(s[:size])
			b.WriteString(q[1 : len(q)-1])
		} else {
			b.WriteString(s[:size])
		}
		s = s[size:]
	}
	return b.String()
}

func needsEscape(r rune) bool {
	return !unicode.IsPrint(r)
}

// clip limits s to limit runes, saying how many were dropped.
func clip(s string, limit int) string {
	n := utf8.RuneCountInString(s)
	if n <= limit {
		return s
	}
	keep := limit - clipReserve
	cut := 0
	for range keep {
		_, size := utf8.DecodeRuneInString(s[cut:])
		cut += size
	}
	return s[:cut] + fmt.Sprintf(" …[truncated, %d chars]", n-keep)
}

// entry is a summary line body: the logical line ID first, so clipping never removes it.
func entry(id int, text string) string {
	return clip(fmt.Sprintf("#%d %s", id, text), maxEntry)
}
