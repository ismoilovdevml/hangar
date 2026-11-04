package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Log levels of run log lines.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Run logs are stored as text, one line per entry:
// "<RFC 3339 time> <LEVEL> <message>\n". Messages never contain newlines.
func formatLogLine(at time.Time, level, msg string) string {
	return at.UTC().Format(time.RFC3339) + " " + strings.ToUpper(level) + " " +
		strings.NewReplacer("\r", " ", "\n", " ").Replace(msg) + "\n"
}

// LogLine is one parsed line of a run log (schema TaskRunLogLine).
type LogLine struct {
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// RunLog is a slice of a run log (schema TaskRunLog).
type RunLog struct {
	Items []LogLine `json:"items"`
	// Truncated: older lines were dropped when the log hit its size limit.
	Truncated bool `json:"truncated"`
}

func parseLogLine(s string) (LogLine, bool) {
	ts, rest, ok := strings.Cut(s, " ")
	if !ok {
		return LogLine{}, false
	}
	at, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return LogLine{}, false
	}
	l := LogLine{At: at.UTC(), Level: LevelInfo, Message: rest}
	if lvl, msg, ok := strings.Cut(rest, " "); ok {
		switch strings.ToLower(lvl) {
		case LevelDebug, LevelInfo, LevelWarn, LevelError:
			l.Level, l.Message = strings.ToLower(lvl), msg
		}
	}
	return l, true
}

// parseLog splits a stored log into numbered lines. When the log was cut to
// its size limit the first (partial) line is dropped and truncated is set;
// sequence numbers then count from the oldest line still stored.
func parseLog(text string, limitBytes int) (lines []LogLine, truncated bool) {
	if text == "" {
		return nil, false
	}
	parts := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(text) >= limitBytes-1 {
		truncated = true
	}
	seq := 0
	for i, p := range parts {
		l, ok := parseLogLine(p)
		if !ok {
			if i == 0 {
				truncated = true // cut in the middle of a line
				continue
			}
			l = LogLine{Level: LevelInfo, Message: p}
		}
		seq++
		l.Seq = seq
		lines = append(lines, l)
	}
	return lines, truncated
}

// Default and maximum lines per RunLog call.
const (
	DefaultLogLines = 1000
	MaxLogLines     = 5000
)

// RunLog returns the log lines of a run with seq > after, oldest first, at
// most limit (0 = DefaultLogLines).
func (s *Service) RunLog(ctx context.Context, taskRef, runID string, after, limit int) (*RunLog, error) {
	t, err := s.Get(ctx, taskRef)
	if err != nil {
		return nil, err
	}
	if !isUUID(runID) {
		return nil, fmt.Errorf("run %q: %w", runID, core.ErrNotFound)
	}
	switch {
	case limit <= 0:
		limit = DefaultLogLines
	case limit > MaxLogLines:
		limit = MaxLogLines
	}
	var text string
	err = s.pool.QueryRow(ctx, `SELECT log FROM task_runs WHERE id = $1 AND task_id = $2`, runID, t.ID).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("run %q: %w", runID, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get run log %q: %w", runID, err)
	}
	lines, truncated := parseLog(text, s.opts.LogLimitBytes)
	out := &RunLog{Items: []LogLine{}, Truncated: truncated}
	for _, l := range lines {
		if l.Seq > after {
			out.Items = append(out.Items, l)
			if len(out.Items) == limit {
				break
			}
		}
	}
	return out, nil
}

// summarize fills Run.ComponentsDeleted and Run.BytesFreed from the result
// (or the checkpoint of a running run) of the task types that report them.
func summarize(r *Run, result []byte) {
	if len(result) == 0 {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(result, &m) != nil {
		return
	}
	if inner, ok := m["result"]; ok { // a checkpoint wraps the running totals
		var mm map[string]json.RawMessage
		if json.Unmarshal(inner, &mm) == nil {
			m = mm
		}
	}
	num := func(k string) *int64 {
		var v int64
		if raw, ok := m[k]; ok && json.Unmarshal(raw, &v) == nil {
			return &v
		}
		return nil
	}
	switch r.TaskType {
	case TypeCleanup:
		r.ComponentsDeleted, r.BytesFreed = num("deleted"), num("bytes")
	case TypeDockerGC:
		r.BytesFreed = num("bytes")
	case TypeBlobCompaction:
		r.BytesFreed = num("freed_bytes")
	}
}
