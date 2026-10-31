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

	"pass-approval/internal/channel"
	"pass-approval/internal/rules"
	"pass-approval/internal/token"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Q is the query surface shared by a connection and a transaction.
type Q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// Conn can start a transaction. Decide and Redeem need that.
type Conn interface {
	Q
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Channels maps a channel name ("telegram", "discord", "whatsapp", "test") to its sender.
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

// delaysMin is the callback retry wait, in minutes, then the row is dead.
// 1, 5, 15, 60, 180, 360, 720.
var delaysMin = []int{1, 5, 15, 60, 180, 360, 720}

// Tenant reads Authorization: Bearer <api key>, hashes the key, and returns the tenant id.
// Return false when the header is missing or the hash matches no tenant.
func Tenant(ctx context.Context, db Q, header string) (string, bool) {
	const p = "Bearer "
	if len(header) < len(p) || header[:len(p)] != p {
		return "", false
	}
	var id string
	err := db.QueryRow(ctx, `select id from tenants where api_key_hash = $1`, token.Hash(header[len(p):])).Scan(&id)
	return id, err == nil	
}

// Create inserts one request.
// Normalize the rule first. An invalid shape returns the error and inserts nothing.
// Confirm every approver exists for this tenant before the insert. An unknown approver is an error.
// Insert with on conflict (tenant_id, external_ref) do nothing.
// A conflict returns the existing id and status and does not send another card.
// expires_at is now plus ExpiresIn seconds. Status starts as pending.
// Then notify step 0.
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

// Get loads one request owned by this tenant and returns present().
// A missing row is ok=false and a nil error, so the handler can answer 404.
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

// Cancel sets a pending request to cancelled and writes the callback in the same flow.
// A request that is already terminal is returned unchanged.
// A missing id is ok=false.
// After the status change, edit every card to the outcome with no buttons.
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

// LinkCode checks the user belongs to the tenant, then inserts a hashed 6-digit code
// that expires in 10 minutes. Return the raw code, not the hash.
// An unknown user is ok=false.
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

// Redeem marks a matching unused, unexpired link code as used and upserts channel_identities.
// The unique key is (channel, external_user_id): a relink replaces the user.
// Do this in one transaction. Return false when the code is missing, used, or expired.
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

// Tap is one decision. Action is set only by the fallback page.
// Chat buttons leave Action empty and use the action stored on the token.
type Tap struct {
	Token          string
	ExternalUserID string
	Channel        string
	Action         string
}

// Decide applies one tap inside a single transaction.
// Lock the request row with SELECT ... FOR UPDATE before locking the token.
// Always take those two locks in that order.
// Stop without inserting a decision when the token is missing or already used,
// the request is not pending or is past expires_at, the chat user is not that approver,
// or the token's step is not the open step.
// An action of "open" skips the chat-link check and takes the action from Tap.Action.
// Insert the decision on conflict do nothing, then mark the token used.
// If the new status is not pending, update the request only where status is still pending,
// and insert the callback_outbox row in this same transaction.
// After commit: a terminal request edits every card with the outcome and no buttons.
// A still-pending request edits only the voters' cards, with no buttons.
// If the open step index changed, notify that new step.
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

// Preview loads the request behind an open token.
// found is false when the hash matches no open token.
// unused is true when used_at is still null.
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

// Sweep expires every pending row whose expires_at is in the past.
// For each one, enqueue the callback and edit its cards, then Deliver at most 10 due rows.
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

// Deliver posts due callback_outbox rows.
// Sign HMAC-SHA256 over timestamp + "." + raw body with the tenant callback secret.
// Send X-Pass-Timestamp and X-Pass-Signature.
// A 2xx deletes the row. Any other result increments attempts and waits delaysMin[attempts].
// Past the last delay, set dead.
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

// load reads one request by id and unmarshals fields and steps.
func load(ctx context.Context, db Q, id string) (Request, error) {
	return scanReq(db.QueryRow(ctx, `
		select id, tenant_id, external_ref, title, body, fields, rule, steps, status, callback_url, expires_at, decided_at
		from requests where id = $1`, id))
}

// loadTenant is load plus tenant_id, so one tenant cannot read another's request.
func loadTenant(ctx context.Context, db Q, tenantID, id string) (Request, error) {
	return scanReq(db.QueryRow(ctx, `
		select id, tenant_id, external_ref, title, body, fields, rule, steps, status, callback_url, expires_at, decided_at
		from requests where id = $1 and tenant_id = $2`, id, tenantID))
}

// loadLocked is load with FOR UPDATE. Call it only inside the Decide transaction.
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

// present returns the JSON view: the request plus its decisions ordered by created_at.
// Each decision uses approver, action, channel, comment, and at.
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

// enqueue inserts one callback_outbox row.
// The body is id, external_ref, status, decided_at, and decisions.
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

// notify sends the open step only.
// Skip an approver who has no channel_identities row.
// Insert three action_tokens per approver: approve, reject, and open. Store the hash only.
// The card carries the raw approve and reject tokens, and LinkToken is the raw open token.
// A failed Send is logged and skipped. The request row stays.
// ponytail: a failed card is not retried. The upgrade is an outbox row per card, same shape as callback_outbox.
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

// editAll rewrites every messages row for this request. The card has no buttons.
func editAll(ctx context.Context, db Q, req Request, ch Channels) {
	card := channel.Card{Title: req.Title, Body: req.Body, Fields: append(append([]channel.Field{}, req.Fields...), channel.Field{Label: "Status", Value: req.Status})}
	editQuery(ctx, db, req.ID, card, ch, `
		select channel, external_user_id, message_id from messages where request_id = $1`, req.ID)
}

// editVoted rewrites only the cards of approvers who have already voted.
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