# Rebuild in Go

Port the running service to Go. Keep the HTTP contract, the Neon database, and the five checks in `src/check.ts`. Do not add a form builder, attachments, nested requests, or delegation.

Read [DESIGN.md](DESIGN.md) for the rules and [DEPLOY.md](DEPLOY.md) for the host. The TypeScript tree stays until the Go tests pass and a request created by one server can be decided by the other.

## What does not change

- The tables in `schema.sql`, including `action_tokens.step_index` and the dev tenant.
- Routes: `POST /v1/requests`, `GET /v1/requests/:id`, `POST /v1/requests/:id/cancel`, `POST /v1/link-codes`, the three webhooks, `GET` and `POST /a/:token`, and `GET` or `POST /internal/sweep`.
- Bearer API keys stored as SHA-256. Callbacks signed as HMAC-SHA256 over `timestamp + "." + raw body`.
- Tokens of 128 bits, stored as a hash, single-use.
- Rules `any`, `all`, and `sequence`. Reject is a veto. A sequence notifies the next step only after the current step passes.
- The sweep. Hobby cron is still once a day. Expiry also runs at the start of API traffic.
- Discord: verify Ed25519 on the raw body, answer a ping with `{ "type": 1 }`, and answer a button with `{ "type": 6 }` before any database work.

## Layout

One module at the repo root, next to the TypeScript app. Vercel’s Go runtime looks for `go.mod` there and a server that listens on `PORT`.

```
go.mod
main.go                         listen on PORT, mount routes
internal/db                     one connection, simple protocol
internal/rules                  evaluate, open step, normalize
internal/token                  random id, 128-bit token, SHA-256
internal/channel                Card, plus memory, telegram, discord, whatsapp
internal/store                  create, decide, link, sweep, callbacks
internal/page                   HTML for /a/:token
```

`internal/store` takes a `channel.Channel` and a database handle. It does not import `net/http`. That is the same split as `src/core/store.ts`.

## Dependencies

Use the standard library for HTTP, HMAC, SHA-256, Ed25519, and HTML escaping. Add `github.com/jackc/pgx/v5` for Postgres. Add nothing else unless a chat call cannot be done with `net/http`.

Open the pooled Neon URL with the simple protocol. The pooler is transaction-mode PgBouncer, and a prepared statement does not survive the next query. In the TypeScript app this is `prepare: false` in `src/db.ts`.

```
postgres://…?sslmode=require&default_query_exec_mode=simple_protocol
```

Keep one connection while the process is a short-lived function. A `pgxpool` with a max of 1 is enough.

## Order

Each step is done when its check passes. Stay there until it does.

### 1. Module and database

`go mod init` at the repo root. Read `DATABASE_URL` from the environment. Connect with the simple protocol and run `select 1`.

Done when that query prints a row against the existing Neon database. Do not create a second database and do not alter `schema.sql`.

`go.mod` is already there. Go does not load `.env`. From the repo root, load it, add the driver, then run:

```powershell
go get github.com/jackc/pgx/v5
go mod tidy
Get-Content .env | ForEach-Object { if ($_ -match '^\s*([^#=]+)=(.*)$') { Set-Item -Path "env:$($matches[1].Trim())" -Value $matches[2].Trim() } }
go run .
```

`main.go` for this step only. Replace it when the real server exists. Vercel treats the root `main` package as the program it runs.

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}

	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The Neon pooler does not keep a session, so a prepared statement
	// can disappear before the next query. Send each query in the simple protocol.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	var n int
	if err := conn.QueryRow(ctx, "select 1").Scan(&n); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(n)
}
```

A successful run prints `1`.

### 2. Rules

Create `internal/rules/rules.go`. No database import. `Evaluate` returns `pending`, `approved`, or `rejected`. A reject on the step in hand returns `rejected`. `Open` returns the first unfinished step, or `-1` when the request is already finished. `Normalize` turns `any` and `all` into one step. `sequence` must be a list of `any` or `all` steps.

```go
package rules

import "errors"

type Step struct {
	Rule      string   `json:"rule"`
	Approvers []string `json:"approvers"`
}

type Decision struct {
	StepIndex  int
	ApproverID string
	Action     string
}

func Normalize(rule string, approvers []string, steps []Step) ([]Step, error) {
	if rule == "sequence" {
		if len(steps) == 0 {
			return nil, errors.New("bad request")
		}
		out := make([]Step, len(steps))
		for i, step := range steps {
			clean, err := checkStep(step)
			if err != nil {
				return nil, err
			}
			out[i] = clean
		}
		return out, nil
	}
	if rule != "any" && rule != "all" {
		return nil, errors.New("bad request")
	}
	if len(approvers) == 0 {
		return nil, errors.New("bad request")
	}
	clean, err := checkStep(Step{Rule: rule, Approvers: approvers})
	if err != nil {
		return nil, err
	}
	return []Step{clean}, nil
}

func checkStep(step Step) (Step, error) {
	if step.Rule != "any" && step.Rule != "all" {
		return Step{}, errors.New("bad request")
	}
	if len(step.Approvers) == 0 {
		return Step{}, errors.New("bad request")
	}
	seen := map[string]bool{}
	for _, id := range step.Approvers {
		if seen[id] {
			return Step{}, errors.New("bad request")
		}
		seen[id] = true
	}
	return step, nil
}

func Evaluate(steps []Step, decisions []Decision) string {
	for i, step := range steps {
		mine := filter(decisions, i)
		for _, d := range mine {
			if d.Action == "reject" {
				return "rejected"
			}
		}
		if !passed(step, mine) {
			return "pending"
		}
	}
	return "approved"
}

func Open(steps []Step, decisions []Decision) int {
	if Evaluate(steps, decisions) != "pending" {
		return -1
	}
	for i, step := range steps {
		if !passed(step, filter(decisions, i)) {
			return i
		}
	}
	return -1
}

func filter(decisions []Decision, step int) []Decision {
	var out []Decision
	for _, d := range decisions {
		if d.StepIndex == step {
			out = append(out, d)
		}
	}
	return out
}

func passed(step Step, decisions []Decision) bool {
	ok := map[string]bool{}
	for _, d := range decisions {
		if d.Action == "approve" {
			ok[d.ApproverID] = true
		}
	}
	if step.Rule == "any" {
		return len(ok) > 0
	}
	for _, id := range step.Approvers {
		if !ok[id] {
			return false
		}
	}
	return true
}
```

`internal/rules/rules_test.go`:

```go
package rules

import "testing"

func TestAllNeedsBoth(t *testing.T) {
	steps := []Step{{Rule: "all", Approvers: []string{"a", "b"}}}
	if Evaluate(steps, []Decision{{0, "a", "approve"}}) != "pending" {
		t.Fatal("one vote")
	}
	if Evaluate(steps, []Decision{{0, "a", "approve"}, {0, "b", "approve"}}) != "approved" {
		t.Fatal("both")
	}
	if Open(steps, []Decision{{0, "a", "approve"}}) != 0 {
		t.Fatal("still step 0")
	}
}

func TestRejectVeto(t *testing.T) {
	steps := []Step{{Rule: "any", Approvers: []string{"a", "b"}}}
	if Evaluate(steps, []Decision{{0, "a", "reject"}}) != "rejected" {
		t.Fatal()
	}
}

func TestSequenceHoldsStep2(t *testing.T) {
	steps := []Step{{Rule: "any", Approvers: []string{"a"}}, {Rule: "any", Approvers: []string{"b"}}}
	if Open(steps, nil) != 0 {
		t.Fatal("step 1")
	}
	if Open(steps, []Decision{{0, "a", "approve"}}) != 1 {
		t.Fatal("step 2")
	}
}
```

```powershell
go test ./internal/rules
```

Done when that passes and does not open a database.

### 3. Tokens

Create `internal/token/token.go`. Sixteen random bytes, base64 raw URL encoding, no padding. SHA-256 hex for API keys, link codes, and action tokens. A link code is six digits. Request ids keep the `req_` prefix.

```go
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/big"
)

func NewToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func NewLinkCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return pad6(int(n.Int64()))
}

func pad6(n int) string {
	s := "000000" + itoa(n)
	return s[len(s)-6:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func NewID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
```

`internal/token/token_test.go` checks the token is 22 characters. Telegram allows 64 bytes of callback data.

```go
package token

import "testing"

func TestTokenFitsTelegram(t *testing.T) {
	got := NewToken()
	if len(got) != 22 {
		t.Fatalf("len %d", len(got))
	}
	if Hash("dev-key") == Hash("other") {
		t.Fatal("hash")
	}
}
```

```powershell
go test ./internal/token
```

### 4. Channel

Create `internal/channel/channel.go`. `LinkToken` is the single-use `open` token in the WhatsApp fallback URL. `Verify` is the only method that sees HTTP headers. The store does not build responses.

```go
package channel

import (
	"context"
	"net/http"
)

type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type Action struct {
	Token string
	Label string
	Style string
}

type Card struct {
	Title     string
	Body      string
	Fields    []Field
	Actions   []Action
	LinkToken string
}

type Inbound struct {
	ExternalUserID string
	Token          string
}

type Channel interface {
	Send(ctx context.Context, to string, card Card) (messageID string, err error)
	Edit(ctx context.Context, to, messageID string, card Card) error
	Verify(header http.Header, raw []byte) error
	Parse(body []byte) (Inbound, bool)
}

func Text(card Card) string {
	s := card.Title + "\n\n" + card.Body
	for _, f := range card.Fields {
		s += "\n" + f.Label + ": " + f.Value
	}
	return s
}
```

`internal/channel/memory.go` records sends and edits for the tests.

```go
package channel

import (
	"context"
	"net/http"
)

type Sent struct {
	To   string
	Card Card
}

type Memory struct {
	Sent  []Sent
	Edits []Card
}

func (m *Memory) Send(_ context.Context, to string, card Card) (string, error) {
	m.Sent = append(m.Sent, Sent{To: to, Card: card})
	return "m" + itoa(len(m.Sent)), nil
}

func (m *Memory) Edit(_ context.Context, _, _ string, card Card) error {
	m.Edits = append(m.Edits, card)
	return nil
}

func (m *Memory) Verify(http.Header, []byte) error { return nil }

func (m *Memory) Parse([]byte) (Inbound, bool) { return Inbound{}, false }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
```

Done when `go test ./internal/channel ./internal/rules ./internal/token` passes. The memory type has no test of its own yet. The store tests in the next section use it.

### 5. Store

`internal/db/db.go` opens one connection with the simple protocol. `internal/store/store.go` is create, get, cancel, link, decide, sweep, and deliver. Decide locks the request row with `FOR UPDATE` before the token row. A failed card send is logged and the request stays. The comment in notify names the upgrade: an outbox row per card.

Load `DATABASE_URL` the same way as step 1, then:

```powershell
go test ./internal/store -count=1
```

The five cases use the memory channel and Neon. Each case deletes its tenant at the end.

`internal/db/db.go`:

```go
package db

import (
	"context"
	"errors"
	"os"

	"github.com/jackc/pgx/v5"
)

func Open(ctx context.Context) (*pgx.Conn, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return pgx.ConnectConfig(ctx, cfg)
}
```

`internal/store/store.go`:

```go
package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"pass-approval/internal/channel"
	"pass-approval/internal/rules"
	"pass-approval/internal/token"
)

type Q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

type Conn interface {
	Q
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Channels map[string]channel.Channel

type Request struct {
	ID          string
	TenantID    string
	ExternalRef string
	Title       string
	Body        string
	Fields      []channel.Field
	Rule        string
	Steps       []rules.Step
	Status      string
	CallbackURL string
	ExpiresAt   time.Time
	DecidedAt   *time.Time
}

type CreateInput struct {
	ExternalRef string
	Title       string
	Body        string
	Fields      []channel.Field
	Rule        string
	Approvers   []string
	Steps       []rules.Step
	CallbackURL string
	ExpiresIn   int
}

type Created struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

var delaysMin = []int{1, 5, 15, 60, 180, 360, 720}

func Tenant(ctx context.Context, db Q, header string) (string, bool) {
	const p = "Bearer "
	if len(header) <= len(p) || header[:len(p)] != p {
		return "", false
	}
	var id string
	err := db.QueryRow(ctx, `select id from tenants where api_key_hash = $1`, token.Hash(header[len(p):])).Scan(&id)
	return id, err == nil
}

func Create(ctx context.Context, db Q, tenantID string, in CreateInput, ch Channels) (Created, error) {
	steps, err := rules.Normalize(in.Rule, in.Approvers, in.Steps)
	if err != nil {
		return Created{}, err
	}
	ids := uniqueApprovers(steps)
	rows, err := db.Query(ctx, `select id from users where tenant_id = $1 and id = any($2)`, tenantID, ids)
	if err != nil {
		return Created{}, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if n != len(ids) {
		return Created{}, errors.New("unknown approver")
	}
	fields, _ := json.Marshal(in.Fields)
	rawSteps, _ := json.Marshal(steps)
	id := token.NewID("req_")
	expires := time.Now().Add(time.Duration(in.ExpiresIn) * time.Second)
	var inserted string
	err = db.QueryRow(ctx, `
		insert into requests (id, tenant_id, external_ref, title, body, fields, rule, steps, status, callback_url, expires_at)
		values ($1,$2,$3,$4,$5,$6::jsonb,$7,$8::jsonb,'pending',$9,$10)
		on conflict (tenant_id, external_ref) do nothing
		returning id`,
		id, tenantID, in.ExternalRef, in.Title, in.Body, string(fields), in.Rule, string(rawSteps), in.CallbackURL, expires,
	).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		var status string
		err = db.QueryRow(ctx, `select id, status from requests where tenant_id = $1 and external_ref = $2`, tenantID, in.ExternalRef).Scan(&id, &status)
		return Created{ID: id, Status: status}, err
	}
	if err != nil {
		return Created{}, err
	}
	req, err := load(ctx, db, inserted)
	if err != nil {
		return Created{}, err
	}
	if err := notify(ctx, db, req, 0, ch); err != nil {
		return Created{}, err
	}
	return Created{ID: inserted, Status: "pending"}, nil
}

func Get(ctx context.Context, db Q, tenantID, id string) (map[string]any, bool, error) {
	req, err := loadTenant(ctx, db, tenantID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	view, err := present(ctx, db, req)
	return view, true, err
}

func Cancel(ctx context.Context, db Q, tenantID, id string, ch Channels) (map[string]any, bool, error) {
	req, err := loadTenant(ctx, db, tenantID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if req.Status == "pending" {
		err = db.QueryRow(ctx, `
			update requests set status = 'cancelled', decided_at = now()
			where id = $1 and status = 'pending'
			returning decided_at`, id).Scan(&req.DecidedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, err
		}
		if err == nil {
			req.Status = "cancelled"
			if err := enqueue(ctx, db, req); err != nil {
				return nil, false, err
			}
			editAll(ctx, db, req, ch)
		}
	}
	view, err := present(ctx, db, req)
	return view, true, err
}

func LinkCode(ctx context.Context, db Q, tenantID, userID string) (string, bool, error) {
	var id string
	err := db.QueryRow(ctx, `select id from users where tenant_id = $1 and id = $2`, tenantID, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	code := token.NewLinkCode()
	_, err = db.Exec(ctx, `insert into link_codes (code_hash, tenant_id, user_id, expires_at) values ($1,$2,$3,$4)`,
		token.Hash(code), tenantID, userID, time.Now().Add(10*time.Minute))
	return code, true, err
}

func Redeem(ctx context.Context, conn Conn, chName, externalID, code string) (bool, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var tenantID, userID string
	err = tx.QueryRow(ctx, `
		update link_codes set used_at = now()
		where code_hash = $1 and used_at is null and expires_at > now()
		returning tenant_id, user_id`, token.Hash(code)).Scan(&tenantID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `
		insert into channel_identities (tenant_id, user_id, channel, external_user_id)
		values ($1,$2,$3,$4)
		on conflict (channel, external_user_id) do update
		set tenant_id = excluded.tenant_id, user_id = excluded.user_id, linked_at = now()`,
		tenantID, userID, chName, externalID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func Decide(ctx context.Context, conn Conn, in struct {
	Token, ExternalUserID, Channel, Action string
}, ch Channels) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var requestID string
	err = tx.QueryRow(ctx, `select request_id from action_tokens where token_hash = $1`, token.Hash(in.Token)).Scan(&requestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	req, err := loadLocked(ctx, tx, requestID)
	if err != nil {
		return err
	}
	var approver, action string
	var step int
	var used *time.Time
	err = tx.QueryRow(ctx, `
		select approver_id, action, step_index, used_at from action_tokens
		where token_hash = $1 for update`, token.Hash(in.Token)).Scan(&approver, &action, &step, &used)
	if err != nil {
		return err
	}
	if used != nil || req.Status != "pending" || !req.ExpiresAt.After(time.Now()) {
		_, err = tx.Exec(ctx, `update action_tokens set used_at = now() where token_hash = $1 and used_at is null`, token.Hash(in.Token))
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		if req.Status != "pending" {
			editAll(ctx, conn, req, ch)
		}
		return nil
	}
	chosen := action
	if action == "open" {
		chosen = in.Action
	}
	if chosen != "approve" && chosen != "reject" {
		return tx.Commit(ctx)
	}
	if action != "open" {
		var linked string
		err = tx.QueryRow(ctx, `
			select user_id from channel_identities
			where channel = $1 and external_user_id = $2 and tenant_id = $3`,
			in.Channel, in.ExternalUserID, req.TenantID).Scan(&linked)
		if err != nil || linked != approver {
			return tx.Commit(ctx)
		}
	}
	prior, err := decisionRows(ctx, tx, req.ID)
	if err != nil {
		return err
	}
	if rules.Open(req.Steps, prior) != step || !contains(req.Steps[step].Approvers, approver) {
		return tx.Commit(ctx)
	}
	var inserted string
	err = tx.QueryRow(ctx, `
		insert into decisions (request_id, step_index, approver_id, action, channel, comment)
		values ($1,$2,$3,$4,$5,'')
		on conflict do nothing
		returning request_id`, req.ID, step, approver, chosen, in.Channel).Scan(&inserted)
	_, _ = tx.Exec(ctx, `update action_tokens set used_at = now() where token_hash = $1`, token.Hash(in.Token))
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	prior = append(prior, rules.Decision{StepIndex: step, ApproverID: approver, Action: chosen})
	status := rules.Evaluate(req.Steps, prior)
	if status != "pending" {
		err = tx.QueryRow(ctx, `
			update requests set status = $2, decided_at = now()
			where id = $1 and status = 'pending'
			returning decided_at`, req.ID, status).Scan(&req.DecidedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			req.Status = status
			if err := enqueue(ctx, tx, req); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if req.Status != "pending" {
		editAll(ctx, conn, req, ch)
		return nil
	}
	voted := map[string]bool{}
	for _, d := range prior {
		voted[d.ApproverID] = true
	}
	var ids []string
	for id := range voted {
		ids = append(ids, id)
	}
	editVoted(ctx, conn, req, ids, ch)
	if next := rules.Open(req.Steps, prior); next != -1 && next != step {
		return notify(ctx, conn, req, next, ch)
	}
	return nil
}

func Preview(ctx context.Context, db Q, raw string) (Request, bool, bool, error) {
	var req Request
	var used *time.Time
	var fields, steps []byte
	err := db.QueryRow(ctx, `
		select r.id, r.tenant_id, r.external_ref, r.title, r.body, r.fields, r.rule, r.steps,
		       r.status, r.callback_url, r.expires_at, r.decided_at, t.used_at
		from action_tokens t join requests r on r.id = t.request_id
		where t.token_hash = $1 and t.action = 'open'`, token.Hash(raw)).Scan(
		&req.ID, &req.TenantID, &req.ExternalRef, &req.Title, &req.Body, &fields, &req.Rule, &steps,
		&req.Status, &req.CallbackURL, &req.ExpiresAt, &req.DecidedAt, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, false, false, nil
	}
	if err != nil {
		return Request{}, false, false, err
	}
	_ = json.Unmarshal(fields, &req.Fields)
	_ = json.Unmarshal(steps, &req.Steps)
	return req, true, used == nil, nil
}

func Sweep(ctx context.Context, db Q, ch Channels) error {
	rows, err := db.Query(ctx, `
		update requests set status = 'expired', decided_at = now()
		where status = 'pending' and expires_at < now()
		returning id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		req, err := load(ctx, db, id)
		if err != nil {
			return err
		}
		if err := enqueue(ctx, db, req); err != nil {
			return err
		}
		editAll(ctx, db, req, ch)
	}
	return Deliver(ctx, db, 10)
}

func Deliver(ctx context.Context, db Q, limit int) error {
	rows, err := db.Query(ctx, `
		select o.id, o.body::text, o.attempts, r.callback_url, t.callback_secret
		from callback_outbox o
		join requests r on r.id = o.request_id
		join tenants t on t.id = r.tenant_id
		where o.dead = false and o.next_at <= now()
		order by o.next_at
		limit $1`, limit)
	if err != nil {
		return err
	}
	type due struct {
		id, body, url, secret string
		attempts              int
	}
	var batch []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.body, &d.attempts, &d.url, &d.secret); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, d)
	}
	rows.Close()
	for _, d := range batch {
		ts := time.Now().Unix()
		stamp := itoa64(ts)
		mac := hmac.New(sha256.New, []byte(d.secret))
		_, _ = mac.Write([]byte(stamp + "." + d.body))
		sig := hex.EncodeToString(mac.Sum(nil))
		ok := post(ctx, d.url, d.body, stamp, sig)
		if ok {
			_, err = db.Exec(ctx, `delete from callback_outbox where id = $1`, d.id)
		} else if d.attempts >= len(delaysMin) {
			_, err = db.Exec(ctx, `update callback_outbox set dead = true, attempts = attempts + 1 where id = $1`, d.id)
		} else {
			_, err = db.Exec(ctx, `update callback_outbox set attempts = attempts + 1, next_at = now() + make_interval(mins => $2) where id = $1`, d.id, delaysMin[d.attempts])
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func post(ctx context.Context, url, body, stamp, sig string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-pass-timestamp", stamp)
	req.Header.Set("x-pass-signature", sig)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode >= 200 && res.StatusCode < 300
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func load(ctx context.Context, db Q, id string) (Request, error) {
	return scanReq(db.QueryRow(ctx, `
		select id, tenant_id, external_ref, title, body, fields, rule, steps, status, callback_url, expires_at, decided_at
		from requests where id = $1`, id))
}

func loadTenant(ctx context.Context, db Q, tenantID, id string) (Request, error) {
	return scanReq(db.QueryRow(ctx, `
		select id, tenant_id, external_ref, title, body, fields, rule, steps, status, callback_url, expires_at, decided_at
		from requests where id = $1 and tenant_id = $2`, id, tenantID))
}

func loadLocked(ctx context.Context, db Q, id string) (Request, error) {
	return scanReq(db.QueryRow(ctx, `
		select id, tenant_id, external_ref, title, body, fields, rule, steps, status, callback_url, expires_at, decided_at
		from requests where id = $1 for update`, id))
}

func scanReq(row pgx.Row) (Request, error) {
	var r Request
	var fields, steps []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.ExternalRef, &r.Title, &r.Body, &fields, &r.Rule, &steps, &r.Status, &r.CallbackURL, &r.ExpiresAt, &r.DecidedAt)
	if err != nil {
		return Request{}, err
	}
	_ = json.Unmarshal(fields, &r.Fields)
	_ = json.Unmarshal(steps, &r.Steps)
	return r, nil
}

func present(ctx context.Context, db Q, req Request) (map[string]any, error) {
	rows, err := db.Query(ctx, `
		select approver_id, action, channel, comment, created_at
		from decisions where request_id = $1 order by created_at`, req.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var decisions []map[string]any
	for rows.Next() {
		var approver, action, ch, comment string
		var at time.Time
		if err := rows.Scan(&approver, &action, &ch, &comment, &at); err != nil {
			return nil, err
		}
		decisions = append(decisions, map[string]any{
			"approver": approver, "action": action, "channel": ch, "comment": comment, "at": at.UTC().Format(time.RFC3339),
		})
	}
	var decided any
	if req.DecidedAt != nil {
		decided = req.DecidedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id": req.ID, "external_ref": req.ExternalRef, "title": req.Title, "body": req.Body,
		"fields": req.Fields, "rule": req.Rule, "steps": req.Steps, "status": req.Status,
		"callback_url": req.CallbackURL, "expires_at": req.ExpiresAt.UTC().Format(time.RFC3339),
		"decided_at": decided, "decisions": decisions,
	}, nil
}

func enqueue(ctx context.Context, db Q, req Request) error {
	view, err := present(ctx, db, req)
	if err != nil {
		return err
	}
	var decided any
	if req.DecidedAt != nil {
		decided = req.DecidedAt.UTC().Format(time.RFC3339)
	}
	body := map[string]any{
		"id": req.ID, "external_ref": req.ExternalRef, "status": req.Status,
		"decided_at": decided, "decisions": view["decisions"],
	}
	raw, _ := json.Marshal(body)
	_, err = db.Exec(ctx, `insert into callback_outbox (request_id, body) values ($1, $2::jsonb)`, req.ID, string(raw))
	return err
}

func notify(ctx context.Context, db Q, req Request, step int, ch Channels) error {
	for _, approver := range req.Steps[step].Approvers {
		rows, err := db.Query(ctx, `
			select channel, external_user_id from channel_identities
			where tenant_id = $1 and user_id = $2`, req.TenantID, approver)
		if err != nil {
			return err
		}
		type ident struct{ ch, ext string }
		var ids []ident
		for rows.Next() {
			var one ident
			if err := rows.Scan(&one.ch, &one.ext); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, one)
		}
		rows.Close()
		if len(ids) == 0 {
			continue
		}
		raw := map[string]string{}
		for _, action := range []string{"approve", "reject", "open"} {
			raw[action] = token.NewToken()
			_, err = db.Exec(ctx, `
				insert into action_tokens (token_hash, request_id, approver_id, action, step_index)
				values ($1,$2,$3,$4,$5)`, token.Hash(raw[action]), req.ID, approver, action, step)
			if err != nil {
				return err
			}
		}
		card := channel.Card{
			Title: req.Title, Body: req.Body, Fields: req.Fields, LinkToken: raw["open"],
			Actions: []channel.Action{
				{Token: raw["approve"], Label: "Approve", Style: "primary"},
				{Token: raw["reject"], Label: "Reject", Style: "danger"},
			},
		}
		for _, one := range ids {
			c := ch[one.ch]
			if c == nil {
				continue
			}
			// ponytail: a failed send is not retried. Upgrade path is an outbox row per card.
			msgID, err := c.Send(ctx, one.ext, card)
			if err != nil {
				log.Printf("send failed %s: %v", one.ch, err)
				continue
			}
			_, err = db.Exec(ctx, `
				insert into messages (request_id, channel, external_user_id, message_id)
				values ($1,$2,$3,$4)
				on conflict (request_id, channel, external_user_id) do update set message_id = excluded.message_id`,
				req.ID, one.ch, one.ext, msgID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func decisionRows(ctx context.Context, db Q, id string) ([]rules.Decision, error) {
	rows, err := db.Query(ctx, `select step_index, approver_id, action from decisions where request_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rules.Decision
	for rows.Next() {
		var d rules.Decision
		if err := rows.Scan(&d.StepIndex, &d.ApproverID, &d.Action); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func editAll(ctx context.Context, db Q, req Request, ch Channels) {
	card := channel.Card{Title: req.Title, Body: req.Body, Fields: append(append([]channel.Field{}, req.Fields...), channel.Field{Label: "Status", Value: req.Status})}
	editQuery(ctx, db, req.ID, card, ch, `
		select channel, external_user_id, message_id from messages where request_id = $1`, req.ID)
}

func editVoted(ctx context.Context, db Q, req Request, approvers []string, ch Channels) {
	if len(approvers) == 0 {
		return
	}
	card := channel.Card{Title: req.Title, Body: req.Body, Fields: append(append([]channel.Field{}, req.Fields...), channel.Field{Label: "Your vote", Value: "recorded"})}
	editQuery(ctx, db, req.ID, card, ch, `
		select m.channel, m.external_user_id, m.message_id
		from messages m
		join channel_identities i on i.channel = m.channel and i.external_user_id = m.external_user_id and i.tenant_id = $2
		where m.request_id = $1 and i.user_id = any($3)`, req.ID, req.TenantID, approvers)
}

func editQuery(ctx context.Context, db Q, _ string, card channel.Card, ch Channels, sql string, args ...any) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		log.Printf("edit query: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name, ext, msg string
		if err := rows.Scan(&name, &ext, &msg); err != nil {
			return
		}
		if c := ch[name]; c != nil {
			if err := c.Edit(ctx, ext, msg, card); err != nil {
				log.Printf("edit failed %s: %v", name, err)
			}
		}
	}
}

func uniqueApprovers(steps []rules.Step) []string {
	seen := map[string]bool{}
	var out []string
	for _, step := range steps {
		for _, id := range step.Approvers {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func contains(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}
```

`internal/store/store_test.go`:

```go
package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"pass-approval/internal/channel"
	"pass-approval/internal/db"
	"pass-approval/internal/rules"
	"pass-approval/internal/token"
)

func TestChecks(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	t.Run("all", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		created := mustCreate(t, ctx, conn, fx, input("all", []string{"ada", "bob"}, nil))
		if len(fx.mem.Sent) != 2 {
			t.Fatalf("sent %d", len(fx.mem.Sent))
		}
		tok := approve(t, fx.mem, "ext_ada")
		mustDecide(t, ctx, conn, fx, tok, "ext_ada")
		if status(t, ctx, conn, fx, created.ID) != "pending" || decisions(t, ctx, conn, fx, created.ID) != 1 {
			t.Fatal("after ada")
		}
		mustDecide(t, ctx, conn, fx, tok, "ext_ada")
		if decisions(t, ctx, conn, fx, created.ID) != 1 {
			t.Fatal("replay")
		}
		mustDecide(t, ctx, conn, fx, approve(t, fx.mem, "ext_bob"), "ext_bob")
		if status(t, ctx, conn, fx, created.ID) != "approved" || decisions(t, ctx, conn, fx, created.ID) != 2 {
			t.Fatal("after bob")
		}
	})

	t.Run("sequence", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		created := mustCreate(t, ctx, conn, fx, input("sequence", nil, []rules.Step{
			{Rule: "any", Approvers: []string{"ada"}},
			{Rule: "any", Approvers: []string{"bob"}},
		}))
		if len(fx.mem.Sent) != 1 || fx.mem.Sent[0].To != "ext_ada" {
			t.Fatal("step 1 only")
		}
		mustDecide(t, ctx, conn, fx, approve(t, fx.mem, "ext_ada"), "ext_ada")
		if status(t, ctx, conn, fx, created.ID) != "pending" || len(fx.mem.Sent) != 2 {
			t.Fatal("step 2")
		}
	})

	t.Run("reject", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		created := mustCreate(t, ctx, conn, fx, input("any", []string{"ada", "bob"}, nil))
		mustDecide(t, ctx, conn, fx, reject(t, fx.mem, "ext_ada"), "ext_ada")
		if status(t, ctx, conn, fx, created.ID) != "rejected" || decisions(t, ctx, conn, fx, created.ID) != 1 {
			t.Fatal("veto")
		}
	})

	t.Run("repeat", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		in := input("any", []string{"ada"}, nil)
		first := mustCreate(t, ctx, conn, fx, in)
		n := len(fx.mem.Sent)
		second := mustCreate(t, ctx, conn, fx, in)
		if second.ID != first.ID || len(fx.mem.Sent) != n {
			t.Fatal("repeat sent again")
		}
	})

	t.Run("unlinked", func(t *testing.T) {
		fx := fixture(t, ctx, conn)
		defer fx.cleanup()
		created := mustCreate(t, ctx, conn, fx, input("any", []string{"ada"}, nil))
		if _, err := conn.Exec(ctx, `delete from channel_identities where tenant_id = $1`, fx.tenant); err != nil {
			t.Fatal(err)
		}
		mustDecide(t, ctx, conn, fx, approve(t, fx.mem, "ext_ada"), "ext_ada")
		if status(t, ctx, conn, fx, created.ID) != "pending" || decisions(t, ctx, conn, fx, created.ID) != 0 {
			t.Fatal("unlinked wrote a decision")
		}
	})
}

type fx struct {
	tenant  string
	mem     *channel.Memory
	ch      Channels
	cleanup func()
}

func fixture(t *testing.T, ctx context.Context, conn *pgx.Conn) *fx {
	t.Helper()
	id := token.NewID("ten_")
	if _, err := conn.Exec(ctx, `insert into tenants (id, name, api_key_hash, callback_secret) values ($1,'check',$1,'secret')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `insert into users (id, tenant_id, name) values ('ada',$1,'Ada'), ('bob',$1,'Bob')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		insert into channel_identities (tenant_id, user_id, channel, external_user_id)
		values ($1,'ada','test','ext_ada'), ($1,'bob','test','ext_bob')`, id); err != nil {
		t.Fatal(err)
	}
	mem := &channel.Memory{}
	return &fx{
		tenant: id, mem: mem, ch: Channels{"test": mem},
		cleanup: func() { _, _ = conn.Exec(ctx, `delete from tenants where id = $1`, id) },
	}
}

func input(rule string, approvers []string, steps []rules.Step) CreateInput {
	return CreateInput{
		ExternalRef: token.NewID("ref_"), Title: "Deploy", Body: "sha",
		Fields: []channel.Field{{Label: "Env", Value: "prod"}},
		Rule:   rule, Approvers: approvers, Steps: steps,
		CallbackURL: "https://example.com/hook", ExpiresIn: 3600,
	}
}

func mustCreate(t *testing.T, ctx context.Context, conn *pgx.Conn, fx *fx, in CreateInput) Created {
	t.Helper()
	created, err := Create(ctx, conn, fx.tenant, in, fx.ch)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func mustDecide(t *testing.T, ctx context.Context, conn *pgx.Conn, fx *fx, raw, ext string) {
	t.Helper()
	err := Decide(ctx, conn, struct {
		Token, ExternalUserID, Channel, Action string
	}{raw, ext, "test", "approve"}, fx.ch)
	if err != nil {
		t.Fatal(err)
	}
}

func approve(t *testing.T, m *channel.Memory, to string) string {
	t.Helper()
	return action(t, m, to, "Approve")
}

func reject(t *testing.T, m *channel.Memory, to string) string {
	t.Helper()
	return action(t, m, to, "Reject")
}

func action(t *testing.T, m *channel.Memory, to, label string) string {
	t.Helper()
	for _, s := range m.Sent {
		if s.To != to {
			continue
		}
		for _, a := range s.Card.Actions {
			if a.Label == label {
				return a.Token
			}
		}
	}
	t.Fatalf("missing %s for %s", label, to)
	return ""
}

func status(t *testing.T, ctx context.Context, conn *pgx.Conn, fx *fx, id string) string {
	t.Helper()
	view, ok, err := Get(ctx, conn, fx.tenant, id)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	return view["status"].(string)
}

func decisions(t *testing.T, ctx context.Context, conn *pgx.Conn, fx *fx, id string) int {
	t.Helper()
	view, ok, err := Get(ctx, conn, fx.tenant, id)
	if err != nil || !ok {
		t.Fatal(err)
	}
	list, _ := view["decisions"].([]map[string]any)
	return len(list)
}
```

### 6. HTTP

Replace the step 1 `main.go` with the server below. It listens on `PORT`, or `8080` when `PORT` is empty. Sweep runs at the start of `/v1` and of `/a/{token}`. `/internal/sweep` accepts GET and POST and requires `Authorization: Bearer <CRON_SECRET>`.

A bad create body is 400. An unknown approver is 400. A bad key is 401. An id this tenant does not own is 404. `html/template` escapes the fallback page.

Done when a create returns `{ "id", "status": "pending" }` and a second call with that `external_ref` returns the same id. PowerShell still needs `curl.exe --%` so the JSON quotes survive.

`main.go`:

```go
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"pass-approval/internal/channel"
	"pass-approval/internal/db"
	"pass-approval/internal/page"
	"pass-approval/internal/rules"
	"pass-approval/internal/store"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", authed(create))
	mux.HandleFunc("GET /v1/requests/{id}", authed(get))
	mux.HandleFunc("POST /v1/requests/{id}/cancel", authed(cancel))
	mux.HandleFunc("POST /v1/link-codes", authed(linkCode))
	mux.HandleFunc("POST /webhooks/telegram", telegramHook)
	mux.HandleFunc("POST /webhooks/discord", discordHook)
	mux.HandleFunc("GET /webhooks/whatsapp", whatsappChallenge)
	mux.HandleFunc("POST /webhooks/whatsapp", whatsappHook)
	mux.HandleFunc("GET /a/{token}", openPage)
	mux.HandleFunc("POST /a/{token}", openPage)
	mux.HandleFunc("GET /internal/sweep", cronSweep)
	mux.HandleFunc("POST /internal/sweep", cronSweep)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func channels() store.Channels {
	return store.Channels{
		"telegram": channel.Telegram{},
		"discord":  channel.Discord{},
		"whatsapp": channel.WhatsApp{},
	}
}

func open(w http.ResponseWriter, r *http.Request) (*pgx.Conn, bool) {
	conn, err := db.Open(r.Context())
	if err != nil {
		http.Error(w, `{"error":"internal"}`, 500)
		return nil, false
	}
	return conn, true
}

func authed(next func(http.ResponseWriter, *http.Request, *pgx.Conn, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, ok := open(w, r)
		if !ok {
			return
		}
		defer conn.Close(r.Context())
		if err := store.Sweep(r.Context(), conn, channels()); err != nil {
			log.Print(err)
			http.Error(w, `{"error":"internal"}`, 500)
			return
		}
		tenant, ok := store.Tenant(r.Context(), conn, r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		next(w, r, conn, tenant)
	}
}

func create(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, tenant string) {
	in, ok := parseCreate(r.Body)
	if !ok {
		http.Error(w, `{"error":"bad request"}`, 400)
		return
	}
	created, err := store.Create(r.Context(), conn, tenant, in, channels())
	if err != nil {
		if err.Error() == "unknown approver" || err.Error() == "bad request" {
			http.Error(w, `{"error":"bad request"}`, 400)
			return
		}
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	writeJSON(w, 201, created)
}

func get(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, tenant string) {
	view, ok, err := store.Get(r.Context(), conn, tenant, r.PathValue("id"))
	if err != nil {
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	if !ok {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	writeJSON(w, 200, view)
}

func cancel(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, tenant string) {
	view, ok, err := store.Cancel(r.Context(), conn, tenant, r.PathValue("id"), channels())
	if err != nil {
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	if !ok {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	_ = store.Deliver(r.Context(), conn, 10)
	writeJSON(w, 200, view)
}

func linkCode(w http.ResponseWriter, r *http.Request, conn *pgx.Conn, tenant string) {
	var body struct {
		User string `json:"user"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || strings.TrimSpace(body.User) == "" {
		http.Error(w, `{"error":"bad request"}`, 400)
		return
	}
	code, ok, err := store.LinkCode(r.Context(), conn, tenant, strings.TrimSpace(body.User))
	if err != nil {
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	if !ok {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	writeJSON(w, 200, map[string]string{"code": code})
}

func telegramHook(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	tg := channel.Telegram{}
	if err := tg.Verify(r.Header, raw); err != nil {
		w.WriteHeader(401)
		return
	}
	var body struct {
		Message struct {
			Text string `json:"text"`
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
		} `json:"message"`
		CallbackQuery struct {
			ID string `json:"id"`
		} `json:"callback_query"`
	}
	_ = json.Unmarshal(raw, &body)
	conn, ok := open(w, r)
	if !ok {
		return
	}
	defer conn.Close(r.Context())
	text := strings.TrimSpace(body.Message.Text)
	if body.Message.From.ID != 0 && strings.HasPrefix(text, "/link ") && len(strings.Fields(text)) == 2 && len(strings.Fields(text)[1]) == 6 {
		linked, err := store.Redeem(r.Context(), conn, "telegram", itoa(body.Message.From.ID), strings.Fields(text)[1])
		if err != nil {
			log.Print(err)
		}
		title, note := "Not linked", "That code is expired or already used."
		if linked {
			title, note = "Linked", "Approval requests will arrive here."
		}
		_, _ = tg.Send(r.Context(), itoa(body.Message.From.ID), channel.Card{Title: title, Body: note})
		w.WriteHeader(200)
		return
	}
	if body.CallbackQuery.ID != "" {
		_ = channel.AnswerCallback(r.Context(), body.CallbackQuery.ID)
	}
	in, ok := tg.Parse(raw)
	if ok {
		_ = store.Sweep(r.Context(), conn, channels())
		_ = store.Decide(r.Context(), conn, struct{ Token, ExternalUserID, Channel, Action string }{in.Token, in.ExternalUserID, "telegram", ""}, channels())
		_ = store.Deliver(r.Context(), conn, 10)
	}
	w.WriteHeader(200)
}

func discordHook(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	dc := channel.Discord{}
	if err := dc.Verify(r.Header, raw); err != nil {
		w.WriteHeader(401)
		return
	}
	var body struct {
		Type int `json:"type"`
		Data struct {
			Name    string `json:"name"`
			Options []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"options"`
		} `json:"data"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Member struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"member"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Type == 1 {
		writeJSON(w, 200, map[string]int{"type": 1})
		return
	}
	if body.Type == 2 && body.Data.Name == "link" {
		code := ""
		for _, o := range body.Data.Options {
			if o.Name == "code" {
				code = o.Value
			}
		}
		user := body.Member.User.ID
		if user == "" {
			user = body.User.ID
		}
		conn, ok := open(w, r)
		if !ok {
			return
		}
		defer conn.Close(r.Context())
		linked, err := store.Redeem(r.Context(), conn, "discord", user, code)
		if err != nil {
			log.Print(err)
		}
		content := "That code is expired or already used."
		if linked {
			content = "Linked. Approval requests will arrive here."
		}
		writeJSON(w, 200, map[string]any{"type": 4, "data": map[string]any{"content": content, "flags": 64}})
		return
	}
	in, ok := dc.Parse(raw)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"type":6}`))
	if !ok {
		return
	}
	// ponytail: r.Context() is cancelled when this handler returns, so the decision uses a fresh context.
	// If the Vercel Go runtime freezes the process on return, this goroutine does not finish. The upgrade is a decision that fits inside Discord's 3 seconds once Neon is warm.
	go func() {
		ctx := context.Background()
		conn, err := db.Open(ctx)
		if err != nil {
			log.Print(err)
			return
		}
		defer conn.Close(ctx)
		_ = store.Sweep(ctx, conn, channels())
		_ = store.Decide(ctx, conn, struct{ Token, ExternalUserID, Channel, Action string }{in.Token, in.ExternalUserID, "discord", ""}, channels())
		_ = store.Deliver(ctx, conn, 10)
	}()
}

func whatsappChallenge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") == "subscribe" && q.Get("hub.verify_token") == os.Getenv("WHATSAPP_VERIFY_TOKEN") {
		_, _ = w.Write([]byte(q.Get("hub.challenge")))
		return
	}
	w.WriteHeader(403)
}

func whatsappHook(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	wa := channel.WhatsApp{}
	if err := wa.Verify(r.Header, raw); err != nil {
		w.WriteHeader(401)
		return
	}
	msg := channel.FirstMessage(raw)
	conn, ok := open(w, r)
	if !ok {
		return
	}
	defer conn.Close(r.Context())
	if msg != nil && msg.Type == "text" {
		text := strings.TrimSpace(msg.Text.Body)
		parts := strings.Fields(text)
		if msg.From != "" && len(parts) == 2 && parts[0] == "/link" && len(parts[1]) == 6 {
			linked, err := store.Redeem(r.Context(), conn, "whatsapp", msg.From, parts[1])
			if err != nil {
				log.Print(err)
			}
			title, note := "Not linked", "That code is expired or already used."
			if linked {
				title, note = "Linked", "Approval requests will arrive here."
			}
			_, _ = wa.Send(r.Context(), msg.From, channel.Card{Title: title, Body: note})
			w.WriteHeader(200)
			return
		}
	}
	in, ok := wa.Parse(raw)
	if ok {
		_ = store.Sweep(r.Context(), conn, channels())
		_ = store.Decide(r.Context(), conn, struct{ Token, ExternalUserID, Channel, Action string }{in.Token, in.ExternalUserID, "whatsapp", ""}, channels())
		_ = store.Deliver(r.Context(), conn, 10)
	}
	w.WriteHeader(200)
}

func openPage(w http.ResponseWriter, r *http.Request) {
	conn, ok := open(w, r)
	if !ok {
		return
	}
	defer conn.Close(r.Context())
	if err := store.Sweep(r.Context(), conn, channels()); err != nil {
		log.Print(err)
	}
	tok := r.PathValue("token")
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		action := r.FormValue("action")
		if action != "approve" && action != "reject" {
			w.WriteHeader(400)
			_ = page.Write(w, page.View{Gone: true})
			return
		}
		_ = store.Decide(r.Context(), conn, struct{ Token, ExternalUserID, Channel, Action string }{tok, "", "whatsapp", action}, channels())
		_ = store.Deliver(r.Context(), conn, 10)
	}
	req, found, unused, err := store.Preview(r.Context(), conn, tok)
	if err != nil {
		log.Print(err)
		http.Error(w, "internal", 500)
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if !found {
		_ = page.Write(w, page.View{Gone: true})
		return
	}
	_ = page.Write(w, page.View{
		Title: req.Title, Body: req.Body, Fields: req.Fields, Status: req.Status,
		Buttons: req.Status == "pending" && unused && req.ExpiresAt.After(time.Now()),
	})
}

func cronSweep(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("CRON_SECRET")
	got := r.Header.Get("Authorization")
	want := "Bearer " + secret
	if secret == "" || len(got) != len(want) || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		w.WriteHeader(401)
		return
	}
	conn, ok := open(w, r)
	if !ok {
		return
	}
	defer conn.Close(r.Context())
	if err := store.Sweep(r.Context(), conn, channels()); err != nil {
		log.Print(err)
		http.Error(w, `{"error":"internal"}`, 500)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func parseCreate(body io.Reader) (store.CreateInput, bool) {
	var raw struct {
		ExternalRef string          `json:"external_ref"`
		Title       string          `json:"title"`
		Body        string          `json:"body"`
		Fields      []channel.Field `json:"fields"`
		Rule        string          `json:"rule"`
		Approvers   []string        `json:"approvers"`
		Steps       []rules.Step    `json:"steps"`
		CallbackURL string          `json:"callback_url"`
		ExpiresIn   float64         `json:"expires_in"`
	}
	if json.NewDecoder(body).Decode(&raw) != nil {
		return store.CreateInput{}, false
	}
	ref := clipText(raw.ExternalRef, 200)
	title := clipText(raw.Title, 200)
	callback, ok := httpsURL(raw.CallbackURL)
	expires := int(raw.ExpiresIn)
	if ref == "" || title == "" || !ok || expires < 1 || expires > 60*60*24*366 || float64(expires) != raw.ExpiresIn {
		return store.CreateInput{}, false
	}
	return store.CreateInput{
		ExternalRef: ref, Title: title, Body: clipText(raw.Body, 4000),
		Fields: raw.Fields, Rule: raw.Rule, Approvers: raw.Approvers, Steps: raw.Steps,
		CallbackURL: callback, ExpiresIn: expires,
	}, true
}

func clipText(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > max {
		return ""
	}
	return s
}

func httpsURL(s string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
```

`internal/page/page.go`:

```go
package page

import (
	"html/template"
	"io"

	"pass-approval/internal/channel"
)

var layout = template.Must(template.New("page").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pass</title>
{{if .Gone}}<p>This link is no longer active.</p>{{else}}
<h1>{{.Title}}</h1>
<p>{{.Body}}</p>
{{range .Fields}}<p><strong>{{.Label}}</strong> {{.Value}}</p>{{end}}
{{if .Buttons}}
<form method="post">
<button name="action" value="approve" type="submit">Approve</button>
<button name="action" value="reject" type="submit">Reject</button>
</form>
{{else}}<p>Status: {{.Status}}</p>{{end}}
{{end}}`))

type View struct {
	Gone    bool
	Title   string
	Body    string
	Fields  []channel.Field
	Status  string
	Buttons bool
}

func Write(w io.Writer, view View) error {
	return layout.Execute(w, view)
}
```

### 7. Telegram

`X-Telegram-Bot-Api-Secret-Token` is compared in constant time. A tap is a `callback_query`, and the token is the whole `callback_data`. `answerCallbackQuery` runs before decide so the spinner clears. `/link` plus a 6-digit code is a normal message.

`internal/channel/telegram.go`:

```go
package channel

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
)

type Telegram struct{}

func (Telegram) Verify(h http.Header, _ []byte) error {
	a := []byte(os.Getenv("TELEGRAM_WEBHOOK_SECRET"))
	b := []byte(h.Get("X-Telegram-Bot-Api-Secret-Token"))
	if len(a) != len(b) || subtle.ConstantTimeCompare(a, b) != 1 {
		return errors.New("unauthorized")
	}
	return nil
}

func (Telegram) Parse(body []byte) (Inbound, bool) {
	var msg struct {
		CallbackQuery struct {
			Data string `json:"data"`
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
		} `json:"callback_query"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.CallbackQuery.Data == "" || msg.CallbackQuery.From.ID == 0 {
		return Inbound{}, false
	}
	return Inbound{ExternalUserID: strconv.FormatInt(msg.CallbackQuery.From.ID, 10), Token: msg.CallbackQuery.Data}, true
}

func (Telegram) Send(ctx context.Context, to string, card Card) (string, error) {
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := tg(ctx, "sendMessage", map[string]any{
		"chat_id": to, "text": clip(Text(card), 4000), "reply_markup": keyboard(card),
	}, &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.Result.MessageID, 10), nil
}

func (Telegram) Edit(ctx context.Context, to, messageID string, card Card) error {
	id, _ := strconv.ParseInt(messageID, 10, 64)
	return tg(ctx, "editMessageText", map[string]any{
		"chat_id": to, "message_id": id, "text": clip(Text(card), 4000), "reply_markup": keyboard(card),
	}, nil)
}

func AnswerCallback(ctx context.Context, id string) error {
	return tg(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id}, nil)
}

func keyboard(card Card) map[string]any {
	if len(card.Actions) == 0 {
		return map[string]any{"inline_keyboard": []any{}}
	}
	row := make([]any, len(card.Actions))
	for i, a := range card.Actions {
		row[i] = map[string]string{"text": a.Label, "callback_data": a.Token}
	}
	return map[string]any{"inline_keyboard": []any{row}}
}

func tg(ctx context.Context, method string, body any, dest any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+os.Getenv("TELEGRAM_BOT_TOKEN")+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err = io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	var wrap struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(raw, &wrap) != nil || !wrap.OK {
		return errors.New("telegram " + method)
	}
	if dest != nil {
		return json.Unmarshal(raw, dest)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
```

### 8. Discord

`crypto/ed25519` verifies `X-Signature-Ed25519` over the timestamp bytes followed by the raw body. The public key and the signature are hex.

A ping, type 1, returns `{ "type": 1 }` and does not touch the database. A button, type 3, writes `{ "type": 6 }` before decide. The decision runs in a goroutine with `context.Background()` because the request context is cancelled when the handler returns. If the Go runtime freezes the process on return, that goroutine does not finish. The comment in `discordHook` names that ceiling.

`/link` is a slash command, interaction type 2. The reply is type 4. Register the command once, the same way the TypeScript deploy step does.

`Send` opens a DM, then posts. The stored message id is `channelID:messageID` so `Edit` does not open the DM again.

`internal/channel/discord.go`:

```go
package channel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
)

type Discord struct{}

func (Discord) Verify(h http.Header, raw []byte) error {
	pub, err := hex.DecodeString(os.Getenv("DISCORD_PUBLIC_KEY"))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("unauthorized")
	}
	sig, err := hex.DecodeString(h.Get("X-Signature-Ed25519"))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("unauthorized")
	}
	msg := append([]byte(h.Get("X-Signature-Timestamp")), raw...)
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("unauthorized")
	}
	return nil
}

func (Discord) Parse(body []byte) (Inbound, bool) {
	var msg struct {
		Type int `json:"type"`
		Data struct {
			CustomID string `json:"custom_id"`
		} `json:"data"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Member struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"member"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.Type != 3 || msg.Data.CustomID == "" {
		return Inbound{}, false
	}
	id := msg.Member.User.ID
	if id == "" {
		id = msg.User.ID
	}
	if id == "" {
		return Inbound{}, false
	}
	return Inbound{ExternalUserID: id, Token: msg.Data.CustomID}, true
}

func (Discord) Send(ctx context.Context, to string, card Card) (string, error) {
	ch, err := dm(ctx, to)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := discord(ctx, http.MethodPost, "/channels/"+ch+"/messages", payload(card), &out); err != nil {
		return "", err
	}
	return ch + ":" + out.ID, nil
}

func (Discord) Edit(ctx context.Context, to, messageID string, card Card) error {
	ch, id, ok := strings.Cut(messageID, ":")
	if !ok {
		var err error
		ch, err = dm(ctx, to)
		if err != nil {
			return err
		}
		id = messageID
	}
	return discord(ctx, http.MethodPatch, "/channels/"+ch+"/messages/"+id, payload(card), nil)
}

func dm(ctx context.Context, userID string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := discord(ctx, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": userID}, &out)
	return out.ID, err
}

func payload(card Card) map[string]any {
	body := map[string]any{"content": clip(Text(card), 2000)}
	if len(card.Actions) == 0 {
		body["components"] = []any{}
		return body
	}
	buttons := make([]any, len(card.Actions))
	for i, a := range card.Actions {
		style := 4
		if a.Style == "primary" {
			style = 1
		}
		buttons[i] = map[string]any{"type": 2, "style": style, "label": a.Label, "custom_id": a.Token}
	}
	body["components"] = []any{map[string]any{"type": 1, "components": buttons}}
	return body
}

func discord(ctx context.Context, method, path string, body any, dest any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, "https://discord.com/api/v10"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bot "+os.Getenv("DISCORD_BOT_TOKEN"))
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("discord " + method + " " + path)
	}
	if dest == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(dest)
}
```

### 9. WhatsApp

`X-Hub-Signature-256` is HMAC-SHA256 of the raw body, prefixed with `sha256=`. The GET handler echoes `hub.challenge` when `hub.verify_token` matches.

Inside the 24-hour window, send an interactive message with two reply buttons. On error `131047`, send the utility template whose URL button is the `open` token. `Edit` sends a follow-up text message. The Cloud API does not edit an interactive message. The comment on `Edit` says so.

`/link` is a text message to the business number.

`internal/channel/whatsapp.go`:

```go
package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
)

type WhatsApp struct{}

type WAMessage struct {
	From string `json:"from"`
	Type string `json:"type"`
	Text struct {
		Body string `json:"body"`
	} `json:"text"`
	Interactive struct {
		ButtonReply struct {
			ID string `json:"id"`
		} `json:"button_reply"`
	} `json:"interactive"`
}

func (WhatsApp) Verify(h http.Header, raw []byte) error {
	mac := hmac.New(sha256.New, []byte(os.Getenv("WHATSAPP_APP_SECRET")))
	_, _ = mac.Write(raw)
	want := []byte("sha256=" + hex.EncodeToString(mac.Sum(nil)))
	got := []byte(h.Get("X-Hub-Signature-256"))
	if len(want) != len(got) || subtle.ConstantTimeCompare(want, got) != 1 {
		return errors.New("unauthorized")
	}
	return nil
}

func (WhatsApp) Parse(body []byte) (Inbound, bool) {
	msg := FirstMessage(body)
	if msg == nil || msg.From == "" || msg.Interactive.ButtonReply.ID == "" {
		return Inbound{}, false
	}
	return Inbound{ExternalUserID: msg.From, Token: msg.Interactive.ButtonReply.ID}, true
}

func (w WhatsApp) Send(ctx context.Context, to string, card Card) (string, error) {
	if len(card.Actions) == 0 {
		res, err := graph(ctx, map[string]any{
			"messaging_product": "whatsapp", "to": to, "type": "text",
			"text": map[string]string{"body": clip(Text(card), 1024)},
		})
		if err != nil {
			return "", err
		}
		return firstID(res), nil
	}
	buttons := make([]any, len(card.Actions))
	for i, a := range card.Actions {
		buttons[i] = map[string]any{"type": "reply", "reply": map[string]string{"id": a.Token, "title": a.Label}}
	}
	res, err := graph(ctx, map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "interactive",
		"interactive": map[string]any{
			"type":   "button",
			"body":   map[string]string{"text": clip(Text(card), 1024)},
			"action": map[string]any{"buttons": buttons},
		},
	})
	if err != nil {
		return "", err
	}
	if res.Error != nil && res.Error.Code == 131047 && card.LinkToken != "" {
		res, err = graph(ctx, template(to, card.LinkToken))
		if err != nil {
			return "", err
		}
		return firstID(res), nil
	}
	if res.Error != nil {
		return "", errors.New("whatsapp send failed")
	}
	return firstID(res), nil
}

// ponytail: the Cloud API cannot edit an interactive message. This sends a follow-up text.
// Outside the 24-hour window that text fails, and the /a/ page is the record.
func (WhatsApp) Edit(ctx context.Context, to, _ string, card Card) error {
	_, err := graph(ctx, map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "text",
		"text": map[string]string{"body": clip(Text(card), 1024)},
	})
	return err
}

func FirstMessage(body []byte) *WAMessage {
	var root struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Messages []WAMessage `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Entry) == 0 || len(root.Entry[0].Changes) == 0 || len(root.Entry[0].Changes[0].Value.Messages) == 0 {
		return nil
	}
	return &root.Entry[0].Changes[0].Value.Messages[0]
}

func template(to, tok string) map[string]any {
	return map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "template",
		"template": map[string]any{
			"name": os.Getenv("WHATSAPP_TEMPLATE"), "language": map[string]string{"code": "en"},
			"components": []any{map[string]any{
				"type": "button", "sub_type": "url", "index": "0",
				"parameters": []any{map[string]string{"type": "text", "text": tok}},
			}},
		},
	}
}

type graphResult struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func graph(ctx context.Context, body any) (graphResult, error) {
	raw, _ := json.Marshal(body)
	url := "https://graph.facebook.com/v22.0/" + os.Getenv("WHATSAPP_PHONE_NUMBER_ID") + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return graphResult{}, err
	}
	req.Header.Set("authorization", "Bearer "+os.Getenv("WHATSAPP_TOKEN"))
	req.Header.Set("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return graphResult{}, err
	}
	defer res.Body.Close()
	var out graphResult
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out, nil
}

func firstID(res graphResult) string {
	if len(res.Messages) == 0 || res.Messages[0].ID == "" {
		return "sent"
	}
	return res.Messages[0].ID
}
```

### 10. Deploy

Set the Vercel framework preset to Go. The runtime is in beta on Hobby. The server must listen on `PORT`. Keep the cron entry already in `vercel.json`:

```json
{ "crons": [{ "path": "/internal/sweep", "schedule": "0 8 * * *" }] }
```

Copy the same environment variables the TypeScript project uses: `DATABASE_URL`, `CRON_SECRET`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_WEBHOOK_SECRET`, `DISCORD_PUBLIC_KEY`, `DISCORD_BOT_TOKEN`, `DISCORD_APPLICATION_ID`, `WHATSAPP_TOKEN`, `WHATSAPP_APP_SECRET`, `WHATSAPP_PHONE_NUMBER_ID`, `WHATSAPP_VERIFY_TOKEN`, `WHATSAPP_TEMPLATE`.

Point each bot at the new host only after `go test` passes and a local create works. Discord's interactions endpoint sends a ping first. That ping must succeed before you register the slash command.

Leave the TypeScript deployment up until one real request is created, tapped, and called back on the Go host.

## Leave these alone

- Do not add an admin UI, Slack, or email in this port.
- Do not raise the connection pool above 1 while the URL is the Neon pooler.
- Do not schedule the Vercel cron more than once a day on Hobby. A free pinger may call `/internal/sweep` with `CRON_SECRET` when you want expiry closer to the deadline.
- Do not retry a failed card send inside the create request. That retry is the card outbox, and it is a later change.
