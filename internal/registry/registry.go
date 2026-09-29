// Package registry is the brain's memory: every resource any plugin made,
// what it holds, its tags and relations, the operations that changed it, and
// the API tokens. One SQLite file, in WAL mode; the operator backs it up.
package registry

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the pure-Go SQLite: the image has no C library
)

// Resource states.
const (
	Creating = "creating"
	Ready    = "ready"
	Updating = "updating"
	Deleting = "deleting"
	Deleted  = "deleted"
	Failed   = "failed" // a create that did not happen
	Lost     = "lost"   // the engine no longer has it
)

// Live states hold what they hold: they count against their owner's limits.
var Live = []string{Creating, Ready, Updating, Deleting, Lost}

// Busy states have an operation running on them.
var Busy = []string{Creating, Updating, Deleting}

// Operation kinds and states.
const (
	OpCreate = "create"
	OpDelete = "delete"
	OpAction = "action"

	OpRunning   = "running"
	OpSucceeded = "succeeded"
	OpFailed    = "failed"
)

// ErrNotFound is returned for an id the registry does not hold.
var ErrNotFound = errors.New("not found")

// ErrMoved: a conditional update found the resource in another state than
// the one it expected — someone else acted first.
var ErrMoved = errors.New("the resource moved on")

//go:embed schema/*.sql
var schemaFS embed.FS

// Resource is one row of the registry, with its tags and usage.
type Resource struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Plugin    string            `json:"plugin"`
	Owner     string            `json:"owner"`
	Zone      string            `json:"zone"`
	State     string            `json:"state"`
	Spec      json.RawMessage   `json:"spec"`
	Observed  json.RawMessage   `json:"observed"`
	Choices   map[string]string `json:"choices"`
	Usage     map[string]int64  `json:"usage"`
	Tags      map[string]string `json:"tags"`
	Drift     string            `json:"drift,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	DeletedAt *time.Time        `json:"deleted_at,omitempty"`
}

// Operation is a long action as the person polls it.
type Operation struct {
	ID          string          `json:"id"`
	Owner       string          `json:"owner"`
	ResourceID  string          `json:"resource_id"`
	Kind        string          `json:"kind"`
	Action      string          `json:"action,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	ClientToken string          `json:"client_token,omitempty"`
	RequestHash string          `json:"-"`
	State       string          `json:"state"`
	Error       string          `json:"error,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Attempts    int             `json:"attempts"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
}

// Token is an API token, its secret reduced to a hash.
type Token struct {
	ID        string     `json:"id"`
	Hash      string     `json:"-"`
	Owner     string     `json:"owner"`
	Name      string     `json:"name"`
	Groups    []string   `json:"groups"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Store is the open registry.
type Store struct {
	db  *sql.DB
	Now func() time.Time
}

// Open opens (creating when absent) the registry at path and brings its
// schema up to date.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{db: db, Now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("registry %s: %w", path, err)
	}
	return s, nil
}

// Close closes the file.
func (s *Store) Close() error { return s.db.Close() }

// Ping reports whether the file answers.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// migrate applies every schema file numbered above the file's user_version,
// each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	var have int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&have); err != nil {
		return err
	}
	files, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		n, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(f, "schema/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("schema file %s: not numbered", f)
		}
		if n <= have {
			continue
		}
		body, err := schemaFS.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", f, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		have = n
	}
	return nil
}

// Version is the schema's version.
func (s *Store) Version(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// ---- Transactions -----------------------------------------------------------

type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Tx is one write transaction. Everything admission decides — the usage it
// counts and the rows it writes — happens inside one, so two requests can
// never both fit in the last free unit.
type Tx struct {
	ctx context.Context
	q   querier
	now time.Time
}

// Tx runs f in a write transaction (BEGIN IMMEDIATE: it holds the write lock
// from its first statement) and commits when f returns nil.
func (s *Store) Tx(ctx context.Context, f func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(&Tx{ctx: ctx, q: tx, now: s.Now()}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func parseNullTS(v sql.NullString) *time.Time {
	if !v.Valid {
		return nil
	}
	t := parseTS(v.String)
	return &t
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func rawOr(b json.RawMessage, empty string) string {
	if len(b) == 0 {
		return empty
	}
	return string(b)
}

// ---- Resources --------------------------------------------------------------

// InsertResource writes a new resource with its tags and usage.
func (t *Tx) InsertResource(r *Resource) error {
	r.CreatedAt, r.UpdatedAt = t.now, t.now
	if len(r.Observed) == 0 {
		r.Observed = json.RawMessage("{}")
	}
	if r.Tags == nil {
		r.Tags = map[string]string{}
	}
	if r.Choices == nil {
		r.Choices = map[string]string{}
	}
	if r.Usage == nil {
		r.Usage = map[string]int64{}
	}
	_, err := t.q.ExecContext(t.ctx, `INSERT INTO resources
		(id, type, plugin, owner, zone, state, spec, observed, choices, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Type, r.Plugin, r.Owner, r.Zone, r.State, rawOr(r.Spec, "{}"), rawOr(r.Observed, "{}"),
		mustJSON(nonNilS(r.Choices)), ts(t.now), ts(t.now))
	if err != nil {
		return err
	}
	for k, v := range r.Tags {
		if _, err := t.q.ExecContext(t.ctx, `INSERT INTO tags (resource_id, key, value) VALUES (?, ?, ?)`, r.ID, k, v); err != nil {
			return err
		}
	}
	return t.setUsage(r.ID, r.Usage)
}

// InsertRelation records that one resource refers to another, under the
// name of the field that does ("key_pairs").
func (t *Tx) InsertRelation(from, kind, to string) error {
	_, err := t.q.ExecContext(t.ctx, `INSERT OR IGNORE INTO relations (from_id, kind, to_id) VALUES (?, ?, ?)`, from, kind, to)
	return err
}

// Relations lists what a resource refers to, as (kind, id) pairs.
func (s *Store) Relations(ctx context.Context, from string) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, to_id FROM relations WHERE from_id = ? ORDER BY kind, to_id`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var k, to string
		if err := rows.Scan(&k, &to); err != nil {
			return nil, err
		}
		out = append(out, [2]string{k, to})
	}
	return out, rows.Err()
}

func (t *Tx) setUsage(id string, usage map[string]int64) error {
	if _, err := t.q.ExecContext(t.ctx, `DELETE FROM usage WHERE resource_id = ?`, id); err != nil {
		return err
	}
	for d, n := range usage {
		if _, err := t.q.ExecContext(t.ctx, `INSERT INTO usage (resource_id, dimension, amount) VALUES (?, ?, ?)`, id, d, n); err != nil {
			return err
		}
	}
	return nil
}

// Change is what an operation's end writes on its resource; empty fields
// are left as they are.
type Change struct {
	State    string
	Spec     json.RawMessage
	Observed json.RawMessage
	Usage    map[string]int64
	Choices  map[string]string
	Drift    *string
	// IfState: apply only while the resource is still in this state
	// (ErrMoved otherwise).
	IfState string
}

// Update applies a change to a resource.
func (t *Tx) Update(id string, c Change) error { return t.update(id, c) }

func (t *Tx) update(id string, c Change) error {
	sets := []string{"updated_at = ?"}
	args := []any{ts(t.now)}
	if c.State != "" {
		sets = append(sets, "state = ?")
		args = append(args, c.State)
		if c.State == Deleted {
			sets = append(sets, "deleted_at = ?")
			args = append(args, ts(t.now))
		}
	}
	if len(c.Spec) > 0 {
		sets = append(sets, "spec = ?")
		args = append(args, string(c.Spec))
	}
	if len(c.Observed) > 0 {
		sets = append(sets, "observed = ?")
		args = append(args, string(c.Observed))
	}
	if c.Choices != nil {
		sets = append(sets, "choices = ?")
		args = append(args, mustJSON(c.Choices))
	}
	if c.Drift != nil {
		sets = append(sets, "drift = ?")
		args = append(args, *c.Drift)
	}
	where := " WHERE id = ?"
	args = append(args, id)
	if c.IfState != "" {
		where += " AND state = ?"
		args = append(args, c.IfState)
	}
	res, err := t.q.ExecContext(t.ctx, "UPDATE resources SET "+strings.Join(sets, ", ")+where, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if c.IfState != "" {
			return ErrMoved
		}
		return ErrNotFound
	}
	if c.Usage != nil {
		return t.setUsage(id, c.Usage)
	}
	return nil
}

// Resource reads one resource inside the transaction.
func (t *Tx) Resource(id string) (*Resource, error) { return getResource(t.ctx, t.q, id) }

// Resource reads one resource.
func (s *Store) Resource(ctx context.Context, id string) (*Resource, error) {
	return getResource(ctx, s.db, id)
}

const resourceCols = `seq, id, type, plugin, owner, zone, state, spec, observed, choices, drift, created_at, updated_at, deleted_at`

func scanResource(sc interface{ Scan(...any) error }) (*Resource, int64, error) {
	var r Resource
	var seq int64
	var spec, observed, choices, created, updated string
	var deleted sql.NullString
	if err := sc.Scan(&seq, &r.ID, &r.Type, &r.Plugin, &r.Owner, &r.Zone, &r.State, &spec, &observed, &choices,
		&r.Drift, &created, &updated, &deleted); err != nil {
		return nil, 0, err
	}
	r.Spec, r.Observed = json.RawMessage(spec), json.RawMessage(observed)
	if err := json.Unmarshal([]byte(choices), &r.Choices); err != nil {
		return nil, 0, err
	}
	r.CreatedAt, r.UpdatedAt, r.DeletedAt = parseTS(created), parseTS(updated), parseNullTS(deleted)
	return &r, seq, nil
}

func getResource(ctx context.Context, q querier, id string) (*Resource, error) {
	r, _, err := scanResource(q.QueryRowContext(ctx, "SELECT "+resourceCols+" FROM resources WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := fill(ctx, q, []*Resource{r}); err != nil {
		return nil, err
	}
	return r, nil
}

// fill reads the tags and usage of resources.
func fill(ctx context.Context, q querier, rs []*Resource) error {
	for _, r := range rs {
		r.Tags, r.Usage = map[string]string{}, map[string]int64{}
		rows, err := q.QueryContext(ctx, `SELECT key, value FROM tags WHERE resource_id = ?`, r.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				_ = rows.Close()
				return err
			}
			r.Tags[k] = v
		}
		_ = rows.Close()
		rows, err = q.QueryContext(ctx, `SELECT dimension, amount FROM usage WHERE resource_id = ?`, r.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d string
			var n int64
			if err := rows.Scan(&d, &n); err != nil {
				_ = rows.Close()
				return err
			}
			r.Usage[d] = n
		}
		_ = rows.Close()
	}
	return nil
}

// Filter narrows a listing.
type Filter struct {
	Owner  string   // "" = every owner
	Type   string   // "" = every type
	Zone   string   // "" = every zone
	States []string // empty = the live states
	Tags   map[string]string
	After  int64 // the cursor: rows after this seq
	Limit  int   // default 100, at most 1000
}

// Resources lists resources in the order they were made. next is the cursor
// for the following page, 0 when there is none.
func (s *Store) Resources(ctx context.Context, f Filter) (rs []*Resource, next int64, err error) {
	where := []string{"seq > ?"}
	args := []any{f.After}
	add := func(cond string, v any) { where = append(where, cond); args = append(args, v) }
	if f.Owner != "" {
		add("owner = ?", f.Owner)
	}
	if f.Type != "" {
		add("type = ?", f.Type)
	}
	if f.Zone != "" {
		add("zone = ?", f.Zone)
	}
	states := f.States
	if len(states) == 0 {
		states = Live
	}
	where = append(where, "state IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(states)), ", ")+")")
	for _, st := range states {
		args = append(args, st)
	}
	for _, k := range slices.Sorted(maps.Keys(f.Tags)) {
		where = append(where, "id IN (SELECT resource_id FROM tags WHERE key = ? AND value = ?)")
		args = append(args, k, f.Tags[k])
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, "SELECT "+resourceCols+" FROM resources WHERE "+
		strings.Join(where, " AND ")+" ORDER BY seq LIMIT ?", args...)
	if err != nil {
		return nil, 0, err
	}
	var seqs []int64
	for rows.Next() {
		r, seq, err := scanResource(rows)
		if err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		rs, seqs = append(rs, r), append(seqs, seq)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(rs) > limit {
		rs, next = rs[:limit], seqs[limit-1]
	}
	return rs, next, fill(ctx, s.db, rs)
}

// Usage sums what an owner's live resources hold, per dimension.
func (t *Tx) Usage(owner string) (map[string]int64, error) { return usageOf(t.ctx, t.q, owner) }

// Usage sums what an owner's live resources hold, per dimension.
func (s *Store) Usage(ctx context.Context, owner string) (map[string]int64, error) {
	return usageOf(ctx, s.db, owner)
}

func usageOf(ctx context.Context, q querier, owner string) (map[string]int64, error) {
	args := []any{owner}
	for _, st := range Live {
		args = append(args, st)
	}
	rows, err := q.QueryContext(ctx, `SELECT u.dimension, SUM(u.amount) FROM usage u
		JOIN resources r ON r.id = u.resource_id
		WHERE r.owner = ? AND r.state IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(Live)), ", ")+`)
		GROUP BY u.dimension`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var d string
		var n int64
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out[d] = n
	}
	return out, rows.Err()
}

// Counts is how many resources there are per type and state (the metrics).
func (s *Store) Counts(ctx context.Context) (map[[2]string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT type, state, COUNT(*) FROM resources GROUP BY type, state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]int{}
	for rows.Next() {
		var typ, st string
		var n int
		if err := rows.Scan(&typ, &st, &n); err != nil {
			return nil, err
		}
		out[[2]string{typ, st}] = n
	}
	return out, rows.Err()
}

// ---- Operations -------------------------------------------------------------

// InsertOperation writes a new running operation.
func (t *Tx) InsertOperation(op *Operation) error {
	op.State, op.CreatedAt, op.UpdatedAt = OpRunning, t.now, t.now
	var token any
	if op.ClientToken != "" {
		token = op.ClientToken
	}
	_, err := t.q.ExecContext(t.ctx, `INSERT INTO operations
		(id, owner, resource_id, kind, action, params, client_token, request_hash, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.Owner, op.ResourceID, op.Kind, op.Action, rawOr(op.Params, "{}"), token, op.RequestHash,
		op.State, ts(t.now), ts(t.now))
	return err
}

// Attempt counts one more try of an operation.
func (s *Store) Attempt(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET attempts = attempts + 1, updated_at = ? WHERE id = ?`, ts(s.Now()), id)
	return err
}

// FinishOperation ends an operation.
func (t *Tx) FinishOperation(id, state, errMsg string, result json.RawMessage) error {
	_, err := t.q.ExecContext(t.ctx, `UPDATE operations SET state = ?, error = ?, result = ?, updated_at = ?, finished_at = ?
		WHERE id = ?`, state, errMsg, rawOr(result, "null"), ts(t.now), ts(t.now), id)
	return err
}

const opCols = `id, owner, resource_id, kind, action, params, COALESCE(client_token, ''), request_hash, state, error, result,
	attempts, created_at, updated_at, finished_at`

func scanOp(sc interface{ Scan(...any) error }) (*Operation, error) {
	var op Operation
	var params, result, created, updated string
	var finished sql.NullString
	if err := sc.Scan(&op.ID, &op.Owner, &op.ResourceID, &op.Kind, &op.Action, &params, &op.ClientToken, &op.RequestHash,
		&op.State, &op.Error, &result, &op.Attempts, &created, &updated, &finished); err != nil {
		return nil, err
	}
	op.Params, op.Result = json.RawMessage(params), json.RawMessage(result)
	op.CreatedAt, op.UpdatedAt, op.FinishedAt = parseTS(created), parseTS(updated), parseNullTS(finished)
	return &op, nil
}

// OperationByToken finds the operation an owner's client token was used for;
// nil when it was never used.
func (t *Tx) OperationByToken(owner, token string) (*Operation, error) {
	op, err := scanOp(t.q.QueryRowContext(t.ctx, "SELECT "+opCols+" FROM operations WHERE owner = ? AND client_token = ?", owner, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return op, err
}

// Operation reads one operation.
func (s *Store) Operation(ctx context.Context, id string) (*Operation, error) {
	op, err := scanOp(s.db.QueryRowContext(ctx, "SELECT "+opCols+" FROM operations WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return op, err
}

// Operations lists operations, newest first: an owner's ("" = everyone's),
// or a resource's.
func (s *Store) Operations(ctx context.Context, owner, resourceID string, limit int) ([]*Operation, error) {
	where, args := []string{"1 = 1"}, []any{}
	if owner != "" {
		where, args = append(where, "owner = ?"), append(args, owner)
	}
	if resourceID != "" {
		where, args = append(where, "resource_id = ?"), append(args, resourceID)
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	args = append(args, limit)
	return s.ops(ctx, "SELECT "+opCols+" FROM operations WHERE "+strings.Join(where, " AND ")+" ORDER BY seq DESC LIMIT ?", args...)
}

// Running lists the operations still running, oldest first: at start-up,
// the ones the brain died during.
func (s *Store) Running(ctx context.Context) ([]*Operation, error) {
	return s.ops(ctx, "SELECT "+opCols+" FROM operations WHERE state = ? ORDER BY seq", OpRunning)
}

func (s *Store) ops(ctx context.Context, query string, args ...any) ([]*Operation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Operation
	for rows.Next() {
		op, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// ---- Tokens -----------------------------------------------------------------

// InsertToken writes a new token.
func (s *Store) InsertToken(ctx context.Context, tok *Token) error {
	tok.CreatedAt = s.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens (id, hash, owner, name, groups, scopes, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, tok.ID, tok.Hash, tok.Owner, tok.Name, mustJSON(nonNil(tok.Groups)),
		mustJSON(nonNil(tok.Scopes)), ts(tok.CreatedAt), ts(tok.ExpiresAt))
	return err
}

const tokCols = `id, hash, owner, name, groups, scopes, created_at, expires_at, last_used, revoked_at`

func scanToken(sc interface{ Scan(...any) error }) (*Token, error) {
	var tok Token
	var groups, scopes, created, expires string
	var used, revoked sql.NullString
	if err := sc.Scan(&tok.ID, &tok.Hash, &tok.Owner, &tok.Name, &groups, &scopes, &created, &expires, &used, &revoked); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(groups), &tok.Groups); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(scopes), &tok.Scopes); err != nil {
		return nil, err
	}
	tok.CreatedAt, tok.ExpiresAt = parseTS(created), parseTS(expires)
	tok.LastUsed, tok.RevokedAt = parseNullTS(used), parseNullTS(revoked)
	return &tok, nil
}

// TokenByHash finds a token by its secret's hash.
func (s *Store) TokenByHash(ctx context.Context, hash string) (*Token, error) {
	tok, err := scanToken(s.db.QueryRowContext(ctx, "SELECT "+tokCols+" FROM tokens WHERE hash = ?", hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return tok, err
}

// Tokens lists an owner's tokens ("" = everyone's), newest first.
func (s *Store) Tokens(ctx context.Context, owner string) ([]*Token, error) {
	q, args := "SELECT "+tokCols+" FROM tokens ORDER BY created_at DESC", []any{}
	if owner != "" {
		q, args = "SELECT "+tokCols+" FROM tokens WHERE owner = ? ORDER BY created_at DESC", []any{owner}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		tok, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// TouchToken records a token's use.
func (s *Store) TouchToken(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used = ? WHERE id = ?`, ts(s.Now()), id)
	return err
}

// RevokeToken revokes an owner's token ("" owner = any owner's).
func (s *Store) RevokeToken(ctx context.Context, owner, id string) error {
	q, args := `UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, []any{ts(s.Now()), id}
	if owner != "" {
		q, args = q+" AND owner = ?", append(args, owner)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilS(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
