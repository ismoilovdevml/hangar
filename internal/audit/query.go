package audit

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Query filters List and Export. Empty fields match everything.
type Query struct {
	Actor string
	// Action is an exact action or a prefix ending in "*" ("repository.*").
	Action string
	// Target is an exact target or a prefix ending in "*" ("repo:npm-*").
	Target string
	// From is inclusive, To exclusive.
	From, To time.Time
	Limit    int
	Cursor   string
}

// Export formats.
const (
	FormatCSV  = "csv"
	FormatJSON = "json" // newline-delimited JSON
)

// List returns events newest first, one page at a time.
func (l *Log) List(ctx context.Context, q Query) (core.Page[core.AuditEvent], error) {
	where, args, err := q.where()
	if err != nil {
		return core.Page[core.AuditEvent]{}, err
	}
	if q.Cursor != "" {
		at, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return core.Page[core.AuditEvent]{}, err
		}
		args = append(args, at, id)
		where = append(where, fmt.Sprintf("(at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	limit := core.ClampLimit(q.Limit)
	args = append(args, limit+1)
	sql := selectSQL + whereSQL(where) + fmt.Sprintf(" ORDER BY at DESC, id DESC LIMIT $%d", len(args))
	rows, err := l.pool.Query(ctx, sql, args...)
	if err != nil {
		return core.Page[core.AuditEvent]{}, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	page := core.Page[core.AuditEvent]{Items: []core.AuditEvent{}}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return core.Page[core.AuditEvent]{}, err
		}
		page.Items = append(page.Items, e)
	}
	if err := rows.Err(); err != nil {
		return core.Page[core.AuditEvent]{}, fmt.Errorf("list audit events: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.At, last.ID)
	}
	return page, nil
}

// Export streams every event matching q (Limit and Cursor are ignored),
// oldest first, to w as CSV or newline-delimited JSON. Rows are read from the
// database as they are written, so memory use does not grow with the export.
// If w implements Flush() (http.Flusher), it is flushed periodically.
func (l *Log) Export(ctx context.Context, q Query, format string, w io.Writer) error {
	if format != FormatCSV && format != FormatJSON {
		return fmt.Errorf("export format %q: want csv or json: %w", format, core.ErrInvalid)
	}
	where, args, err := q.where()
	if err != nil {
		return err
	}
	rows, err := l.pool.Query(ctx, selectSQL+whereSQL(where)+" ORDER BY at, id", args...)
	if err != nil {
		return fmt.Errorf("export audit events: %w", err)
	}
	defer rows.Close()
	flusher, _ := w.(interface{ Flush() })
	var cw *csv.Writer
	enc := json.NewEncoder(w)
	if format == FormatCSV {
		cw = csv.NewWriter(w)
		if err := cw.Write([]string{"id", "at", "actor", "action", "target", "outcome", "ip", "details"}); err != nil {
			return err
		}
	}
	n := 0
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return err
		}
		if cw != nil {
			details := ""
			if len(e.Details) > 0 {
				b, _ := json.Marshal(e.Details)
				details = string(b)
			}
			rec := []string{strconv.FormatInt(e.ID, 10), e.At.Format(time.RFC3339Nano), e.Actor, e.Action,
				e.Target, e.Outcome, e.IP, details}
			for i := range rec {
				rec[i] = csvSafe(rec[i])
			}
			if err := cw.Write(rec); err != nil {
				return err
			}
		} else if err := enc.Encode(e); err != nil {
			return err
		}
		if n++; n%500 == 0 {
			if cw != nil {
				cw.Flush()
				if err := cw.Error(); err != nil {
					return err
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("export audit events: %w", err)
	}
	if cw != nil {
		cw.Flush()
		return cw.Error()
	}
	return nil
}

// csvSafe neutralises cells a spreadsheet would evaluate as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

const selectSQL = `SELECT id, at, actor, action, target, outcome, COALESCE(host(ip), ''), details FROM audit_events`

func scanEvent(rows pgx.Rows) (core.AuditEvent, error) {
	var e core.AuditEvent
	if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target, &e.Outcome, &e.IP, &e.Details); err != nil {
		return e, fmt.Errorf("scan audit event: %w", err)
	}
	e.At = e.At.UTC()
	if len(e.Details) == 0 {
		e.Details = nil
	}
	return e, nil
}

func (q Query) where() ([]string, []any, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.Actor != "" {
		add("actor = $%d", q.Actor)
	}
	for _, f := range []struct{ col, v string }{{"action", q.Action}, {"target", q.Target}} {
		switch {
		case f.v == "" || f.v == "*":
		case strings.HasSuffix(f.v, "*"):
			add(f.col+` LIKE $%d ESCAPE '\'`, likeEscape(strings.TrimSuffix(f.v, "*"))+"%")
		default:
			add(f.col+" = $%d", f.v)
		}
	}
	if !q.From.IsZero() {
		add("at >= $%d", q.From.UTC())
	}
	if !q.To.IsZero() {
		add("at < $%d", q.To.UTC())
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.To.After(q.From) {
		return nil, nil, fmt.Errorf("audit query: to must be after from: %w", core.ErrInvalid)
	}
	return where, args, nil
}

func whereSQL(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func encodeCursor(at time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixMicro(), 10) + "." + strconv.FormatInt(id, 10)))
}

func decodeCursor(s string) (time.Time, int64, error) {
	bad := fmt.Errorf("cursor %q: %w", s, core.ErrInvalid)
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, bad
	}
	ts, ids, ok := strings.Cut(string(b), ".")
	if !ok {
		return time.Time{}, 0, bad
	}
	us, err1 := strconv.ParseInt(ts, 10, 64)
	id, err2 := strconv.ParseInt(ids, 10, 64)
	if err1 != nil || err2 != nil {
		return time.Time{}, 0, bad
	}
	return time.UnixMicro(us).UTC(), id, nil
}
