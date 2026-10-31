# Build Pass

Build the v1 in [PRD.md](PRD.md) and [DESIGN.md](DESIGN.md) as a TypeScript app on Vercel and Neon. Hosting limits are in [DEPLOY.md](DEPLOY.md).

The app is one Hono server. `core/` talks to Postgres and a `Channel`. It has no HTTP types. Chat adapters implement `Channel`. Expiry and callback retry are a `sweep` function called at the start of a request and from a daily cron. There is no long-running worker.

Each step ends with something you can run. Do them in order.

You need Node 22 or newer, a [Neon](https://neon.com) project, and a [Vercel](https://vercel.com) account. Telegram, Discord, and WhatsApp setup is in the last steps. The rule check does not call them.

## 1. Project

In an empty directory:

```powershell
npm init -y
npm install hono postgres discord-interactions @vercel/functions
npm install --save-dev typescript @types/node
```

`package.json` needs `"type": "module"` and these scripts. Leave the versions npm wrote.

```json
{
  "type": "module",
  "scripts": {
    "check": "node --env-file=.env --experimental-strip-types --test src/check.ts"
  }
}
```

`tsconfig.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "NodeNext",
    "moduleResolution": "NodeNext",
    "strict": true,
    "noEmit": true,
    "types": ["node"],
    "skipLibCheck": true
  },
  "include": ["src"]
}
```

`.gitignore`:

```
node_modules
.env
.vercel
```

`vercel.json`:

```json
{
  "crons": [
    { "path": "/internal/sweep", "schedule": "0 8 * * *" }
  ]
}
```

Hobby cron runs once a day, sometime during that hour.

Copy the Neon **pooled** connection string into `.env`. The pooler is transaction-mode, so the database client later sets `prepare: false`.

```
DATABASE_URL=postgresql://USER:PASSWORD@HOST-pooler.REGION.aws.neon.tech/neondb?sslmode=require
CRON_SECRET=replace-me
```

Add the chat variables when you reach those steps. Leave them empty for now.

```
TELEGRAM_BOT_TOKEN=
TELEGRAM_WEBHOOK_SECRET=
DISCORD_PUBLIC_KEY=
DISCORD_BOT_TOKEN=
DISCORD_APPLICATION_ID=
WHATSAPP_TOKEN=
WHATSAPP_APP_SECRET=
WHATSAPP_PHONE_NUMBER_ID=
WHATSAPP_VERIFY_TOKEN=
WHATSAPP_TEMPLATE=
```

## 2. Database

Create `schema.sql` and run it in the Neon SQL editor.

`step_index` is on `action_tokens` because a token belongs to one step, and a decision is unique on `(request_id, step_index, approver_id)`.

```sql
create table tenants (
  id text primary key,
  name text not null,
  api_key_hash text not null unique,
  callback_secret text not null
);

create table users (
  id text not null,
  tenant_id text not null references tenants (id) on delete cascade,
  name text not null,
  primary key (tenant_id, id)
);

create table channel_identities (
  tenant_id text not null,
  user_id text not null,
  channel text not null,
  external_user_id text not null,
  linked_at timestamptz not null default now(),
  primary key (tenant_id, user_id, channel),
  unique (channel, external_user_id),
  foreign key (tenant_id, user_id) references users (tenant_id, id) on delete cascade
);

create table requests (
  id text primary key,
  tenant_id text not null references tenants (id) on delete cascade,
  external_ref text not null,
  title text not null,
  body text not null default '',
  fields jsonb not null default '[]'::jsonb,
  rule text not null,
  steps jsonb not null,
  status text not null,
  callback_url text not null,
  expires_at timestamptz not null,
  decided_at timestamptz,
  unique (tenant_id, external_ref)
);

create index requests_pending_expiry on requests (expires_at) where status = 'pending';

create table messages (
  request_id text not null references requests (id) on delete cascade,
  channel text not null,
  external_user_id text not null,
  message_id text not null,
  primary key (request_id, channel, external_user_id)
);

create table decisions (
  request_id text not null references requests (id) on delete cascade,
  step_index integer not null,
  approver_id text not null,
  action text not null,
  channel text not null,
  comment text not null default '',
  created_at timestamptz not null default now(),
  unique (request_id, step_index, approver_id)
);

create table action_tokens (
  token_hash text primary key,
  request_id text not null references requests (id) on delete cascade,
  approver_id text not null,
  action text not null,
  step_index integer not null,
  used_at timestamptz
);

create table callback_outbox (
  id bigint generated always as identity primary key,
  request_id text not null references requests (id) on delete cascade,
  body jsonb not null,
  attempts integer not null default 0,
  next_at timestamptz not null default now(),
  dead boolean not null default false
);

create table link_codes (
  code_hash text primary key,
  tenant_id text not null,
  user_id text not null,
  expires_at timestamptz not null,
  used_at timestamptz,
  foreign key (tenant_id, user_id) references users (tenant_id, id) on delete cascade
);
```

Seed a dev tenant. The API key is `dev-key`. Its SHA-256 is below. v1 has no admin UI, so tenants and users are rows.

```sql
insert into tenants (id, name, api_key_hash, callback_secret)
values (
  'ten_dev',
  'dev',
  '7e9f8fd111802be56c379d597842e29b2cebd35ff2133d431a49fa556a18704e',
  'dev-callback-secret'
);

insert into users (id, tenant_id, name) values
  ('ada', 'ten_dev', 'Ada'),
  ('bob', 'ten_dev', 'Bob'),
  ('chao', 'ten_dev', 'Chao');
```

To hash a different key:

```powershell
node --input-type=module -e "import { createHash } from 'node:crypto'; console.log(createHash('sha256').update('dev-key').digest('hex'))"
```

## 3. Rules, tokens, and the channel type

`src/db.ts`:

```ts
import postgres from "postgres"

const url = process.env.DATABASE_URL
if (!url) throw new Error("DATABASE_URL is required")

// ponytail: prepare:false because Neon's pooler is transaction-mode PgBouncer.
// Named prepared statements stick to a session, and the pooler does not.
export const sql = postgres(url, { ssl: "require", max: 1, prepare: false })

export type Db = ReturnType<typeof postgres>
```

`src/core/channel.ts`:

```ts
export type Field = { label: string; value: string }

export type Card = {
  title: string
  body: string
  fields: Field[]
  actions: { token: string; label: string; style: "primary" | "danger" }[]
  linkToken?: string
}

export type Inbound = {
  externalUserId: string
  token: string
}

// verify returns a promise because Discord's signature check is async.
export interface Channel {
  send(to: string, card: Card): Promise<{ messageId: string }>
  edit(to: string, messageId: string, card: Card): Promise<void>
  verify(headers: Headers, rawBody: Buffer): Promise<void>
  parse(body: unknown): Inbound | null
}

export function renderText(card: Card): string {
  const fields = card.fields.map((field) => `${field.label}: ${field.value}`).join("\n")
  return [card.title, "", card.body, fields].filter((part) => part.length > 0).join("\n")
}
```

`linkToken` is the single-use token in the WhatsApp fallback URL. Telegram and Discord ignore it. The page lets the holder choose Approve or Reject. Chat buttons keep one token per action.

`src/core/rules.ts`:

```ts
export type Step = { rule: "any" | "all"; approvers: string[] }

export type DecisionRow = {
  stepIndex: number
  approverId: string
  action: "approve" | "reject"
}

export function normalizeSteps(rule: string, approvers: string[] | undefined, steps: Step[] | undefined): Step[] {
  if (rule === "sequence") {
    if (!steps?.length) throw new Error("bad request")
    return steps.map(checkStep)
  }
  if (rule !== "any" && rule !== "all") throw new Error("bad request")
  if (!approvers?.length) throw new Error("bad request")
  return [checkStep({ rule, approvers })]
}

function checkStep(step: Step): Step {
  if (step.rule !== "any" && step.rule !== "all") throw new Error("bad request")
  if (!step.approvers?.length) throw new Error("bad request")
  if (new Set(step.approvers).size !== step.approvers.length) throw new Error("bad request")
  return { rule: step.rule, approvers: step.approvers }
}

export function evaluate(steps: Step[], decisions: DecisionRow[]): "pending" | "approved" | "rejected" {
  for (let i = 0; i < steps.length; i++) {
    const mine = decisions.filter((decision) => decision.stepIndex === i)
    if (mine.some((decision) => decision.action === "reject")) return "rejected"
    if (!stepPassed(steps[i], mine)) return "pending"
  }
  return "approved"
}

export function openStepIndex(steps: Step[], decisions: DecisionRow[]): number | null {
  if (evaluate(steps, decisions) !== "pending") return null
  for (let i = 0; i < steps.length; i++) {
    const mine = decisions.filter((decision) => decision.stepIndex === i)
    if (!stepPassed(steps[i], mine)) return i
  }
  return null
}

function stepPassed(step: Step, decisions: DecisionRow[]): boolean {
  const approved = new Set(
    decisions.filter((decision) => decision.action === "approve").map((decision) => decision.approverId),
  )
  if (step.rule === "any") return approved.size > 0
  return step.approvers.every((id) => approved.has(id))
}
```

Reject is a veto on every rule. A sequence step is itself `any` or `all`. `openStepIndex` is the only step that may be notified or tapped.

`src/core/tokens.ts`:

```ts
import { createHash, randomBytes, randomInt } from "node:crypto"

// 128 bits. base64url is 22 characters, under Telegram's 64-byte callback_data limit.
export function newToken(): string {
  return randomBytes(16).toString("base64url")
}

export function hashToken(value: string): string {
  return createHash("sha256").update(value).digest("hex")
}

export function newLinkCode(): string {
  return String(randomInt(0, 1_000_000)).padStart(6, "0")
}

export function newId(prefix: string): string {
  return prefix + randomBytes(12).toString("base64url")
}
```

## 4. Store

`src/core/store.ts` is the create, decide, link, and sweep path.

While a request is `pending`, an edit goes only to approvers who have already voted, and that edit removes their buttons. Everyone else keeps the buttons they were sent. Raw tokens are not stored, so those keyboards cannot be rebuilt. When the request leaves `pending`, every card is rewritten with the outcome and no buttons.

A failed `send` is logged and skipped. The request row stays. A repeat `external_ref` does not send again. The upgrade path is an outbox for cards, same shape as `callback_outbox`.

```ts
import { createHmac } from "node:crypto"
import type { Db } from "../db.ts"
import type { Card, Channel, Field } from "./channel.ts"
import { evaluate, openStepIndex, normalizeSteps, type DecisionRow, type Step } from "./rules.ts"
import { hashToken, newId, newLinkCode, newToken } from "./tokens.ts"

export type RequestRow = {
  id: string
  tenant_id: string
  external_ref: string
  title: string
  body: string
  fields: Field[]
  rule: string
  steps: Step[]
  status: string
  callback_url: string
  expires_at: Date
  decided_at: Date | null
}

type Channels = Record<string, Channel>

const DELAYS_MIN = [1, 5, 15, 60, 180, 360, 720]

export async function tenantFromAuthorization(db: Db, header: string | undefined) {
  if (!header?.startsWith("Bearer ")) return null
  const hash = hashToken(header.slice("Bearer ".length))
  const rows = await db<{ id: string; name: string }[]>`
    select id, name from tenants where api_key_hash = ${hash}
  `
  return rows[0] ?? null
}

export async function createRequest(
  db: Db,
  tenantId: string,
  input: {
    externalRef: string
    title: string
    body: string
    fields: Field[]
    rule: string
    approvers?: string[]
    steps?: Step[]
    callbackUrl: string
    expiresIn: number
  },
  channels: Channels,
) {
  const steps = normalizeSteps(input.rule, input.approvers, input.steps)
  const ids = [...new Set(steps.flatMap((step) => step.approvers))]
  const known = await db<{ id: string }[]>`
    select id from users where tenant_id = ${tenantId} and id in ${db(ids)}
  `
  if (known.length !== ids.length) throw new Error("unknown approver")

  const id = newId("req_")
  const expiresAt = new Date(Date.now() + input.expiresIn * 1000)
  const inserted = await db<{ id: string }[]>`
    insert into requests (
      id, tenant_id, external_ref, title, body, fields, rule, steps, status, callback_url, expires_at
    ) values (
      ${id}, ${tenantId}, ${input.externalRef}, ${input.title}, ${input.body},
      ${db.json(input.fields)}, ${input.rule}, ${db.json(steps)}, 'pending',
      ${input.callbackUrl}, ${expiresAt}
    )
    on conflict (tenant_id, external_ref) do nothing
    returning id
  `
  if (!inserted.length) {
    const existing = await db<{ id: string; status: string }[]>`
      select id, status from requests
      where tenant_id = ${tenantId} and external_ref = ${input.externalRef}
    `
    return existing[0]
  }

  const request = await loadRequest(db, id)
  await notify(db, request, 0, channels)
  return { id, status: "pending" }
}

export async function getRequest(db: Db, tenantId: string, id: string) {
  const rows = await db<RequestRow[]>`
    select * from requests where id = ${id} and tenant_id = ${tenantId}
  `
  if (!rows.length) return null
  return present(db, rows[0])
}

export async function cancelRequest(db: Db, tenantId: string, id: string, channels: Channels) {
  const updated = await db<RequestRow[]>`
    update requests
    set status = 'cancelled', decided_at = now()
    where id = ${id} and tenant_id = ${tenantId} and status = 'pending'
    returning *
  `
  if (updated.length) {
    await enqueue(db, updated[0])
    await editAll(db, updated[0], channels)
  }
  return getRequest(db, tenantId, id)
}

export async function createLinkCode(db: Db, tenantId: string, userId: string) {
  const user = await db`select id from users where tenant_id = ${tenantId} and id = ${userId}`
  if (!user.length) return null
  const code = newLinkCode()
  const expiresAt = new Date(Date.now() + 10 * 60 * 1000)
  await db`
    insert into link_codes (code_hash, tenant_id, user_id, expires_at)
    values (${hashToken(code)}, ${tenantId}, ${userId}, ${expiresAt})
  `
  return code
}

export async function redeemLink(db: Db, channel: string, externalUserId: string, code: string) {
  const hash = hashToken(code.trim())
  return db.begin(async (tx) => {
    const rows = await tx<{ tenant_id: string; user_id: string }[]>`
      update link_codes
      set used_at = now()
      where code_hash = ${hash} and used_at is null and expires_at > now()
      returning tenant_id, user_id
    `
    if (!rows.length) return false
    await tx`
      insert into channel_identities (tenant_id, user_id, channel, external_user_id)
      values (${rows[0].tenant_id}, ${rows[0].user_id}, ${channel}, ${externalUserId})
      on conflict (channel, external_user_id) do update
      set tenant_id = excluded.tenant_id,
          user_id = excluded.user_id,
          linked_at = now()
    `
    return true
  })
}

export async function decide(
  db: Db,
  input: {
    token: string
    externalUserId: string
    channelName: string
    action?: "approve" | "reject"
  },
  channels: Channels,
) {
  const tokenHash = hashToken(input.token)
  const result = await db.begin(async (tx) => {
    const found = await tx<{
      request_id: string
      approver_id: string
      action: string
      step_index: number
      used_at: Date | null
    }[]>`
      select request_id, approver_id, action, step_index, used_at
      from action_tokens
      where token_hash = ${tokenHash}
      for update
    `
    if (!found.length) return { kind: "miss" as const }
    const tokenRow = found[0]
    const request = await loadRequest(tx, tokenRow.request_id)
    if (tokenRow.used_at) return { kind: "ignore" as const, request }

    const expired = new Date(request.expires_at).getTime() <= Date.now()
    if (request.status !== "pending" || expired) {
      await tx`update action_tokens set used_at = now() where token_hash = ${tokenHash}`
      return { kind: "ignore" as const, request }
    }

    const action = tokenRow.action === "open" ? input.action : tokenRow.action
    if (action !== "approve" && action !== "reject") return { kind: "ignore" as const, request }

    if (tokenRow.action !== "open") {
      const link = await tx<{ user_id: string }[]>`
        select user_id from channel_identities
        where channel = ${input.channelName}
          and external_user_id = ${input.externalUserId}
          and tenant_id = ${request.tenant_id}
      `
      if (link[0]?.user_id !== tokenRow.approver_id) return { kind: "ignore" as const, request }
    }

    const prior = await tx<{ step_index: number; approver_id: string; action: "approve" | "reject" }[]>`
      select step_index, approver_id, action
      from decisions
      where request_id = ${request.id}
    `
    const decisions: DecisionRow[] = prior.map((row) => ({
      stepIndex: row.step_index,
      approverId: row.approver_id,
      action: row.action,
    }))
    const open = openStepIndex(request.steps, decisions)
    if (open === null || open !== tokenRow.step_index) return { kind: "ignore" as const, request }
    if (!request.steps[open].approvers.includes(tokenRow.approver_id)) {
      return { kind: "ignore" as const, request }
    }

    const inserted = await tx`
      insert into decisions (request_id, step_index, approver_id, action, channel, comment)
      values (${request.id}, ${open}, ${tokenRow.approver_id}, ${action}, ${input.channelName}, '')
      on conflict do nothing
      returning request_id
    `
    await tx`update action_tokens set used_at = now() where token_hash = ${tokenHash}`
    if (!inserted.length) return { kind: "ignore" as const, request }

    decisions.push({ stepIndex: open, approverId: tokenRow.approver_id, action })
    const status = evaluate(request.steps, decisions)
    if (status !== "pending") {
      const updated = await tx<{ decided_at: Date }[]>`
        update requests
        set status = ${status}, decided_at = now()
        where id = ${request.id} and status = 'pending'
        returning decided_at
      `
      if (updated.length) {
        request.status = status
        request.decided_at = updated[0].decided_at
        await enqueue(tx, request)
      }
    }
    return { kind: "done" as const, request, decisions }
  })

  if (result.kind === "miss") return
  if (result.kind === "ignore") {
    if (result.request.status !== "pending") await editAll(db, result.request, channels)
    return
  }

  if (result.request.status !== "pending") {
    await editAll(db, result.request, channels)
    return
  }

  const voted = [...new Set(result.decisions.map((decision) => decision.approverId))]
  await editVoted(db, result.request, voted, channels)
  const next = openStepIndex(result.request.steps, result.decisions)
  const previous = result.decisions.at(-1)?.stepIndex
  if (next !== null && next !== previous) await notify(db, result.request, next, channels)
}

export async function previewOpenToken(db: Db, token: string) {
  const rows = await db<(RequestRow & { used_at: Date | null })[]>`
    select r.*, t.used_at
    from action_tokens t
    join requests r on r.id = t.request_id
    where t.token_hash = ${hashToken(token)} and t.action = 'open'
  `
  return rows[0] ?? null
}

export async function sweep(db: Db, channels: Channels) {
  const expired = await db<RequestRow[]>`
    update requests
    set status = 'expired', decided_at = now()
    where status = 'pending' and expires_at < now()
    returning *
  `
  for (const request of expired) {
    await enqueue(db, request)
    await editAll(db, request, channels)
  }
  await deliverDue(db)
}

export async function deliverDue(db: Db, limit = 10) {
  const due = await db<{
    id: string
    body: unknown
    attempts: number
    callback_url: string
    callback_secret: string
  }[]>`
    select o.id, o.body, o.attempts, r.callback_url, t.callback_secret
    from callback_outbox o
    join requests r on r.id = o.request_id
    join tenants t on t.id = r.tenant_id
    where o.dead = false and o.next_at <= now()
    order by o.next_at
    limit ${limit}
  `
  for (const row of due) {
    const raw = JSON.stringify(row.body)
    const timestamp = Math.floor(Date.now() / 1000).toString()
    const signature = createHmac("sha256", row.callback_secret).update(`${timestamp}.${raw}`).digest("hex")
    let ok = false
    try {
      const res = await fetch(row.callback_url, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "x-pass-timestamp": timestamp,
          "x-pass-signature": signature,
        },
        body: raw,
      })
      ok = res.ok
    } catch {
      ok = false
    }
    if (ok) {
      await db`delete from callback_outbox where id = ${row.id}`
      continue
    }
    // ponytail: seven delays sum to about 22 hours, then the row is dead.
    // A finer schedule is another array, not a new table.
    if (row.attempts >= DELAYS_MIN.length) {
      await db`update callback_outbox set dead = true, attempts = ${row.attempts + 1} where id = ${row.id}`
      continue
    }
    const minutes = DELAYS_MIN[row.attempts]
    await db`
      update callback_outbox
      set attempts = ${row.attempts + 1},
          next_at = now() + make_interval(mins => ${minutes})
      where id = ${row.id}
    `
  }
}

async function loadRequest(db: Db, id: string): Promise<RequestRow> {
  const rows = await db<RequestRow[]>`select * from requests where id = ${id}`
  if (!rows[0]) throw new Error("missing request")
  return rows[0]
}

async function present(db: Db, request: RequestRow) {
  const decisions = await db<{
    approver_id: string
    action: string
    channel: string
    comment: string
    created_at: Date
  }[]>`
    select approver_id, action, channel, comment, created_at
    from decisions
    where request_id = ${request.id}
    order by created_at
  `
  return {
    id: request.id,
    external_ref: request.external_ref,
    title: request.title,
    body: request.body,
    fields: request.fields,
    rule: request.rule,
    steps: request.steps,
    status: request.status,
    callback_url: request.callback_url,
    expires_at: request.expires_at,
    decided_at: request.decided_at,
    decisions: decisions.map((decision) => ({
      approver: decision.approver_id,
      action: decision.action,
      channel: decision.channel,
      comment: decision.comment,
      at: decision.created_at,
    })),
  }
}

async function enqueue(db: Db, request: RequestRow) {
  const view = await present(db, request)
  const body = {
    id: view.id,
    external_ref: view.external_ref,
    status: request.status,
    decided_at: request.decided_at ? new Date(request.decided_at).toISOString() : null,
    decisions: view.decisions.map((decision) => ({
      ...decision,
      at: new Date(decision.at).toISOString(),
    })),
  }
  await db`
    insert into callback_outbox (request_id, body)
    values (${request.id}, ${db.json(body)})
  `
}

async function notify(db: Db, request: RequestRow, stepIndex: number, channels: Channels) {
  for (const approverId of request.steps[stepIndex].approvers) {
    const identities = await db<{ channel: string; external_user_id: string }[]>`
      select channel, external_user_id
      from channel_identities
      where tenant_id = ${request.tenant_id} and user_id = ${approverId}
    `
    if (!identities.length) continue
    const tokens = (["approve", "reject", "open"] as const).map((action) => ({
      action,
      token: newToken(),
    }))
    for (const token of tokens) {
      await db`
        insert into action_tokens (token_hash, request_id, approver_id, action, step_index)
        values (${hashToken(token.token)}, ${request.id}, ${approverId}, ${token.action}, ${stepIndex})
      `
    }
    const card = pendingCard(request, tokens)
    for (const identity of identities) {
      const channel = channels[identity.channel]
      if (!channel) continue
      try {
        const sent = await channel.send(identity.external_user_id, card)
        await db`
          insert into messages (request_id, channel, external_user_id, message_id)
          values (${request.id}, ${identity.channel}, ${identity.external_user_id}, ${sent.messageId})
          on conflict (request_id, channel, external_user_id)
          do update set message_id = excluded.message_id
        `
      } catch (error) {
        console.error("send failed", identity.channel, error)
      }
    }
  }
}

function pendingCard(request: RequestRow, tokens: { action: string; token: string }[]): Card {
  return {
    title: request.title,
    body: request.body,
    fields: request.fields,
    linkToken: tokens.find((token) => token.action === "open")?.token,
    actions: [
      { token: tokens.find((token) => token.action === "approve")!.token, label: "Approve", style: "primary" },
      { token: tokens.find((token) => token.action === "reject")!.token, label: "Reject", style: "danger" },
    ],
  }
}

function outcomeCard(request: RequestRow): Card {
  return {
    title: request.title,
    body: request.body,
    fields: [...request.fields, { label: "Status", value: request.status }],
    actions: [],
  }
}

async function editAll(db: Db, request: RequestRow, channels: Channels) {
  const messages = await db<{ channel: string; external_user_id: string; message_id: string }[]>`
    select channel, external_user_id, message_id from messages where request_id = ${request.id}
  `
  await editMessages(messages, outcomeCard(request), channels)
}

async function editVoted(db: Db, request: RequestRow, approverIds: string[], channels: Channels) {
  if (!approverIds.length) return
  const messages = await db<{ channel: string; external_user_id: string; message_id: string }[]>`
    select m.channel, m.external_user_id, m.message_id
    from messages m
    join channel_identities i
      on i.channel = m.channel
     and i.external_user_id = m.external_user_id
     and i.tenant_id = ${request.tenant_id}
    where m.request_id = ${request.id}
      and i.user_id in ${db(approverIds)}
  `
  const card: Card = {
    title: request.title,
    body: request.body,
    fields: [...request.fields, { label: "Your vote", value: "recorded" }],
    actions: [],
  }
  await editMessages(messages, card, channels)
}

async function editMessages(
  messages: { channel: string; external_user_id: string; message_id: string }[],
  card: Card,
  channels: Channels,
) {
  for (const message of messages) {
    const channel = channels[message.channel]
    if (!channel) continue
    try {
      await channel.edit(message.external_user_id, message.message_id, card)
    } catch (error) {
      console.error("edit failed", message.channel, error)
    }
  }
}
```

## 5. The check

`src/channels/memory.ts` records cards in memory. The check uses it as the channel named `test`.

```ts
import type { Card, Channel, Inbound } from "../core/channel.ts"

export class MemoryChannel implements Channel {
  sent: { to: string; card: Card }[] = []
  edits: Card[] = []

  async send(to: string, card: Card) {
    this.sent.push({ to, card })
    return { messageId: `m${this.sent.length}` }
  }

  async edit(_to: string, _messageId: string, card: Card) {
    this.edits.push(card)
  }

  async verify() {}

  parse(): Inbound | null {
    return null
  }
}
```

`src/check.ts` is the runnable check from the design, plus reject, a repeat `external_ref`, and an unlinked tap. It writes to the database in `DATABASE_URL` and deletes its tenant afterwards. It does not call Telegram.

```ts
import assert from "node:assert/strict"
import test from "node:test"
import { sql } from "./db.ts"
import { MemoryChannel } from "./channels/memory.ts"
import { createRequest, decide, getRequest } from "./core/store.ts"
import { newId } from "./core/tokens.ts"

test("all approves on the second vote, and a token inserts one decision", async () => {
  const fx = await fixture()
  try {
    const created = await createRequest(sql, fx.tenantId, request("all", ["ada", "bob"]), fx.channels)
    assert.deepEqual(fx.memory.sent.map((send) => send.to).sort(), ["ext_ada", "ext_bob"])

    const token = approveToken(fx.memory, "ext_ada")
    await decide(sql, { token, externalUserId: "ext_ada", channelName: "test" }, fx.channels)
    const afterA = await getRequest(sql, fx.tenantId, created.id)
    assert.equal(afterA?.status, "pending")
    assert.equal(afterA?.decisions.length, 1)

    await decide(sql, { token, externalUserId: "ext_ada", channelName: "test" }, fx.channels)
    const replayed = await getRequest(sql, fx.tenantId, created.id)
    assert.equal(replayed?.decisions.length, 1)
    assert.equal(replayed?.status, "pending")

    await decide(sql, {
      token: approveToken(fx.memory, "ext_bob"),
      externalUserId: "ext_bob",
      channelName: "test",
    }, fx.channels)
    const afterB = await getRequest(sql, fx.tenantId, created.id)
    assert.equal(afterB?.status, "approved")
    assert.equal(afterB?.decisions.length, 2)
  } finally {
    await fx.cleanup()
  }
})

test("a sequence does not notify step 2 until step 1 passes", async () => {
  const fx = await fixture()
  try {
    const created = await createRequest(sql, fx.tenantId, {
      ...request("sequence", []),
      approvers: undefined,
      steps: [
        { rule: "any", approvers: ["ada"] },
        { rule: "any", approvers: ["bob"] },
      ],
    }, fx.channels)
    assert.deepEqual(fx.memory.sent.map((send) => send.to), ["ext_ada"])

    await decide(sql, {
      token: approveToken(fx.memory, "ext_ada"),
      externalUserId: "ext_ada",
      channelName: "test",
    }, fx.channels)
    const view = await getRequest(sql, fx.tenantId, created.id)
    assert.equal(view?.status, "pending")
    assert.ok(fx.memory.sent.some((send) => send.to === "ext_bob"))
  } finally {
    await fx.cleanup()
  }
})

test("reject is a veto", async () => {
  const fx = await fixture()
  try {
    const created = await createRequest(sql, fx.tenantId, request("any", ["ada", "bob"]), fx.channels)
    const token = fx.memory.sent.find((send) => send.to === "ext_ada")!.card.actions.find((action) => action.label === "Reject")!.token
    await decide(sql, { token, externalUserId: "ext_ada", channelName: "test" }, fx.channels)
    const view = await getRequest(sql, fx.tenantId, created.id)
    assert.equal(view?.status, "rejected")
    assert.equal(view?.decisions.length, 1)
  } finally {
    await fx.cleanup()
  }
})

test("a repeat external_ref does not send another card", async () => {
  const fx = await fixture()
  try {
    const input = request("any", ["ada"])
    const first = await createRequest(sql, fx.tenantId, input, fx.channels)
    const sent = fx.memory.sent.length
    const second = await createRequest(sql, fx.tenantId, input, fx.channels)
    assert.equal(second.id, first.id)
    assert.equal(fx.memory.sent.length, sent)
  } finally {
    await fx.cleanup()
  }
})

test("an unlinked tap inserts nothing", async () => {
  const fx = await fixture()
  try {
    const created = await createRequest(sql, fx.tenantId, request("any", ["ada"]), fx.channels)
    await sql`delete from channel_identities where tenant_id = ${fx.tenantId}`
    await decide(sql, {
      token: approveToken(fx.memory, "ext_ada"),
      externalUserId: "ext_ada",
      channelName: "test",
    }, fx.channels)
    const view = await getRequest(sql, fx.tenantId, created.id)
    assert.equal(view?.status, "pending")
    assert.equal(view?.decisions.length, 0)
  } finally {
    await fx.cleanup()
  }
})

function request(rule: string, approvers: string[]) {
  return {
    externalRef: newId("ref_"),
    title: "Deploy billing-api",
    body: "sha 9f3c1a by ada",
    fields: [{ label: "Env", value: "prod" }],
    rule,
    approvers,
    callbackUrl: "https://example.com/hook",
    expiresIn: 3600,
  }
}

function approveToken(memory: MemoryChannel, to: string) {
  const card = memory.sent.find((send) => send.to === to)?.card
  const token = card?.actions.find((action) => action.label === "Approve")?.token
  if (!token) throw new Error(`no approve token for ${to}`)
  return token
}

async function fixture() {
  const tenantId = newId("ten_")
  await sql`
    insert into tenants (id, name, api_key_hash, callback_secret)
    values (${tenantId}, 'check', ${tenantId}, 'secret')
  `
  await sql`
    insert into users (id, tenant_id, name) values
      ('ada', ${tenantId}, 'Ada'),
      ('bob', ${tenantId}, 'Bob')
  `
  await sql`
    insert into channel_identities (tenant_id, user_id, channel, external_user_id) values
      (${tenantId}, 'ada', 'test', 'ext_ada'),
      (${tenantId}, 'bob', 'test', 'ext_bob')
  `
  const memory = new MemoryChannel()
  return {
    tenantId,
    memory,
    channels: { test: memory } as Record<string, MemoryChannel>,
    async cleanup() {
      await sql`delete from tenants where id = ${tenantId}`
    },
  }
}
```

Run:

```powershell
npm run check
```

Five tests pass. A connection error here is `DATABASE_URL` or the schema. Fix that before writing HTTP.

## 6. Chat adapters

Each adapter turns a `Card` into a provider payload and turns a webhook into an `Inbound`. Linking and the decision call stay in the HTTP layer.

`src/channels/telegram.ts`:

```ts
import { timingSafeEqual } from "node:crypto"
import { renderText, type Card, type Channel, type Inbound } from "../core/channel.ts"

export function telegramChannel(): Channel {
  const api = () => `https://api.telegram.org/bot${process.env.TELEGRAM_BOT_TOKEN}`

  return {
    async verify(headers) {
      const expected = process.env.TELEGRAM_WEBHOOK_SECRET ?? ""
      const got = headers.get("x-telegram-bot-api-secret-token") ?? ""
      const a = Buffer.from(expected)
      const b = Buffer.from(got)
      if (a.length !== b.length || !timingSafeEqual(a, b)) throw new Error("unauthorized")
    },

    parse(body): Inbound | null {
      const query = (body as { callback_query?: { data?: string; from?: { id?: number } } }).callback_query
      if (!query?.data || query.from?.id == null) return null
      return { externalUserId: String(query.from.id), token: query.data }
    },

    async send(to, card) {
      const json = await call(api(), "sendMessage", {
        chat_id: to,
        text: renderText(card).slice(0, 4000),
        reply_markup: keyboard(card),
      })
      return { messageId: String(json.result.message_id) }
    },

    async edit(to, messageId, card) {
      await call(api(), "editMessageText", {
        chat_id: to,
        message_id: Number(messageId),
        text: renderText(card).slice(0, 4000),
        reply_markup: keyboard(card),
      })
    },
  }
}

export async function answerCallback(callbackQueryId: string) {
  await call(`https://api.telegram.org/bot${process.env.TELEGRAM_BOT_TOKEN}`, "answerCallbackQuery", {
    callback_query_id: callbackQueryId,
  })
}

function keyboard(card: Card) {
  if (!card.actions.length) return { inline_keyboard: [] }
  return {
    inline_keyboard: [card.actions.map((action) => ({ text: action.label, callback_data: action.token }))],
  }
}

async function call(api: string, method: string, body: unknown) {
  const res = await fetch(`${api}/${method}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  })
  const json = await res.json()
  if (!json.ok) throw new Error(`telegram ${method} failed`)
  return json
}
```

The person must open the bot and tap Start once. Until they do, `sendMessage` fails and the request stays `pending` with no card.

`src/channels/discord.ts`:

```ts
import { verifyKey } from "discord-interactions"
import { renderText, type Card, type Channel, type Inbound } from "../core/channel.ts"

const api = "https://discord.com/api/v10"

export function discordChannel(): Channel {
  return {
    async verify(headers, rawBody) {
      const ok = await verifyKey(
        rawBody,
        headers.get("x-signature-ed25519") ?? "",
        headers.get("x-signature-timestamp") ?? "",
        process.env.DISCORD_PUBLIC_KEY ?? "",
      )
      if (!ok) throw new Error("unauthorized")
    },

    parse(body): Inbound | null {
      const interaction = body as {
        type?: number
        data?: { custom_id?: string }
        user?: { id?: string }
        member?: { user?: { id?: string } }
      }
      if (interaction.type !== 3 || !interaction.data?.custom_id) return null
      const externalUserId = interaction.member?.user?.id ?? interaction.user?.id
      if (!externalUserId) return null
      return { externalUserId, token: interaction.data.custom_id }
    },

    async send(to, card) {
      const channelId = await dmChannel(to)
      const json = await discordFetch(`/channels/${channelId}/messages`, payload(card))
      return { messageId: String(json.id) }
    },

    async edit(to, messageId, card) {
      const channelId = await dmChannel(to)
      await discordFetch(`/channels/${channelId}/messages/${messageId}`, payload(card), "PATCH")
    },
  }
}

async function dmChannel(userId: string) {
  const json = await discordFetch("/users/@me/channels", { recipient_id: userId })
  return String(json.id)
}

function payload(card: Card) {
  const body: { content: string; components?: unknown[] } = {
    content: renderText(card).slice(0, 2000),
  }
  if (card.actions.length) {
    body.components = [{
      type: 1,
      components: card.actions.map((action) => ({
        type: 2,
        style: action.style === "primary" ? 1 : 4,
        label: action.label,
        custom_id: action.token,
      })),
    }]
  }
  return body
}

async function discordFetch(path: string, body: unknown, method = "POST") {
  const res = await fetch(api + path, {
    method,
    headers: {
      authorization: `Bot ${process.env.DISCORD_BOT_TOKEN}`,
      "content-type": "application/json",
    },
    body: JSON.stringify(body),
  })
  if (!res.ok) throw new Error(`discord ${method} ${path} failed`)
  return res.json()
}
```

A bot can DM someone who shares a server with it and who allows DMs from server members. v1 does not post in a channel.

Discord's interactions URL receives slash commands, not plain text. `/link` is a slash command with a `code` option. The HTTP step responds to it. Register the command after deploy.

`src/channels/whatsapp.ts`:

```ts
import { createHmac, timingSafeEqual } from "node:crypto"
import { renderText, type Card, type Channel, type Inbound } from "../core/channel.ts"

const graph = "https://graph.facebook.com/v22.0"

export function whatsappChannel(): Channel {
  return {
    async verify(headers, rawBody) {
      const expected = Buffer.from("sha256=" + createHmac("sha256", process.env.WHATSAPP_APP_SECRET ?? "").update(rawBody).digest("hex"))
      const got = Buffer.from(headers.get("x-hub-signature-256") ?? "")
      if (expected.length !== got.length || !timingSafeEqual(expected, got)) throw new Error("unauthorized")
    },

    parse(body): Inbound | null {
      const message = firstMessage(body)
      const token = message?.interactive?.button_reply?.id
      if (!message?.from || !token) return null
      return { externalUserId: message.from, token }
    },

    async send(to, card) {
      if (!card.actions.length) {
        const json = await graphSend({
          messaging_product: "whatsapp",
          to,
          type: "text",
          text: { body: renderText(card).slice(0, 1024) },
        })
        return { messageId: json.messages?.[0]?.id ?? "text" }
      }
      const interactive = await graphSend({
        messaging_product: "whatsapp",
        to,
        type: "interactive",
        interactive: {
          type: "button",
          body: { text: renderText(card).slice(0, 1024) },
          action: {
            buttons: card.actions.map((action) => ({
              type: "reply",
              reply: { id: action.token, title: action.label },
            })),
          },
        },
      })
      if (interactive.error?.code === 131047 && card.linkToken) {
        const fallback = await graphSend(template(to, card.linkToken))
        return { messageId: fallback.messages?.[0]?.id ?? "template" }
      }
      if (interactive.error) throw new Error("whatsapp send failed")
      return { messageId: interactive.messages?.[0]?.id ?? "interactive" }
    },

    // ponytail: the Cloud API has no edit for an interactive message.
    // A follow-up text is the outcome. Outside the 24-hour window it will fail,
    // and the fallback page is the record. Upgrade path is a real edit API, if Meta ships one.
    async edit(to, _messageId, card) {
      await graphSend({
        messaging_product: "whatsapp",
        to,
        type: "text",
        text: { body: renderText(card).slice(0, 1024) },
      })
    },
  }
}

export function firstMessage(body: unknown): {
  from?: string
  type?: string
  text?: { body?: string }
  interactive?: { button_reply?: { id?: string } }
} | null {
  const root = body as {
    entry?: { changes?: { value?: { messages?: unknown[] } }[] }[]
  }
  const message = root.entry?.[0]?.changes?.[0]?.value?.messages?.[0]
  return (message as ReturnType<typeof firstMessage>) ?? null
}

function template(to: string, token: string) {
  return {
    messaging_product: "whatsapp",
    to,
    type: "template",
    template: {
      name: process.env.WHATSAPP_TEMPLATE,
      language: { code: "en" },
      components: [{
        type: "button",
        sub_type: "url",
        index: "0",
        parameters: [{ type: "text", text: token }],
      }],
    },
  }
}

async function graphSend(body: unknown) {
  const res = await fetch(`${graph}/${process.env.WHATSAPP_PHONE_NUMBER_ID}/messages`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${process.env.WHATSAPP_TOKEN}`,
      "content-type": "application/json",
    },
    body: JSON.stringify(body),
  })
  return res.json() as Promise<{ messages?: { id: string }[]; error?: { code?: number } }>
}
```

Change `v22.0` if the Meta dashboard shows a different Graph version. Error `131047` means the 24-hour window is closed, so the adapter sends the utility template. The template is created in Meta: body text such as "You have an approval request.", one URL button, URL `https://YOUR-HOST/a/{{1}}`.

## 7. HTTP

`src/page.ts` renders the fallback page. Title, body, and fields are escaped. The token in the URL is the capability.

```ts
import type { Field } from "./core/channel.ts"

export function pageHtml(view: {
  gone?: boolean
  title?: string
  body?: string
  fields?: Field[]
  status?: string
  buttons?: boolean
}): string {
  if (view.gone || !view.title) {
    return layout("<p>This link is no longer active.</p>")
  }
  const fields = (view.fields ?? [])
    .map((field) => `<p><strong>${escapeHtml(field.label)}</strong> ${escapeHtml(field.value)}</p>`)
    .join("")
  const buttons = view.buttons
    ? `<form method="post"><button name="action" value="approve" type="submit">Approve</button>
       <button name="action" value="reject" type="submit">Reject</button></form>`
    : `<p>Status: ${escapeHtml(view.status ?? "")}</p>`
  return layout(`<h1>${escapeHtml(view.title)}</h1><p>${escapeHtml(view.body ?? "")}</p>${fields}${buttons}`)
}

function layout(body: string) {
  return `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Pass</title>${body}`
}

function escapeHtml(value: string) {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
}
```

`src/index.ts` is the Vercel entry. Export the Hono app as the default export.

```ts
import { Hono } from "hono"
import { sql } from "./db.ts"
import { pageHtml } from "./page.ts"
import { discordChannel } from "./channels/discord.ts"
import { answerCallback, telegramChannel } from "./channels/telegram.ts"
import { firstMessage, whatsappChannel } from "./channels/whatsapp.ts"
import {
  cancelRequest,
  createLinkCode,
  createRequest,
  decide,
  deliverDue,
  getRequest,
  previewOpenToken,
  redeemLink,
  sweep,
  tenantFromAuthorization,
} from "./core/store.ts"

const app = new Hono<{ Variables: { tenantId: string } }>()

function channels() {
  return {
    telegram: telegramChannel(),
    discord: discordChannel(),
    whatsapp: whatsappChannel(),
  }
}

app.use("/v1/*", async (c, next) => {
  await sweep(sql, channels())
  const tenant = await tenantFromAuthorization(sql, c.req.header("authorization"))
  if (!tenant) return c.json({ error: "unauthorized" }, 401)
  c.set("tenantId", tenant.id)
  await next()
})

app.post("/v1/requests", async (c) => {
  const body = await c.req.json().catch(() => null)
  const input = parseCreate(body)
  if (!input) return c.json({ error: "bad request" }, 400)
  try {
    const created = await createRequest(sql, c.get("tenantId"), input, channels())
    return c.json(created, 201)
  } catch (error) {
    if (error instanceof Error && error.message === "unknown approver") {
      return c.json({ error: "unknown approver" }, 400)
    }
    if (error instanceof Error && error.message === "bad request") {
      return c.json({ error: "bad request" }, 400)
    }
    console.error(error)
    return c.json({ error: "internal" }, 500)
  }
})

app.get("/v1/requests/:id", async (c) => {
  const request = await getRequest(sql, c.get("tenantId"), c.req.param("id"))
  if (!request) return c.json({ error: "not found" }, 404)
  return c.json(request)
})

app.post("/v1/requests/:id/cancel", async (c) => {
  const request = await cancelRequest(sql, c.get("tenantId"), c.req.param("id"), channels())
  if (!request) return c.json({ error: "not found" }, 404)
  await deliverDue(sql)
  return c.json(request)
})

app.post("/v1/link-codes", async (c) => {
  const body = await c.req.json().catch(() => null)
  const user = body && typeof body.user === "string" ? body.user : ""
  const code = await createLinkCode(sql, c.get("tenantId"), user)
  if (!code) return c.json({ error: "not found" }, 404)
  return c.json({ code })
})

app.post("/webhooks/telegram", async (c) => {
  const raw = Buffer.from(await c.req.arrayBuffer())
  const telegram = telegramChannel()
  try {
    await telegram.verify(c.req.raw.headers, raw)
  } catch {
    return c.body(null, 401)
  }
  const body = JSON.parse(raw.toString()) as {
    message?: { text?: string; from?: { id?: number } }
    callback_query?: { id?: string }
  }
  const text = body.message?.text?.trim() ?? ""
  const from = body.message?.from?.id
  if (from != null && /^\/link\s+\d{6}$/.test(text)) {
    const ok = await redeemLink(sql, "telegram", String(from), text.split(/\s+/)[1])
    await telegram.send(String(from), {
      title: ok ? "Linked" : "Not linked",
      body: ok ? "Approval requests will arrive here." : "That code is expired or already used.",
      fields: [],
      actions: [],
    })
    return c.body(null, 200)
  }
  if (body.callback_query?.id) await answerCallback(body.callback_query.id)
  const inbound = telegram.parse(body)
  if (inbound) {
    await sweep(sql, channels())
    await decide(sql, { ...inbound, channelName: "telegram" }, channels())
    await deliverDue(sql)
  }
  return c.body(null, 200)
})

app.post("/webhooks/discord", async (c) => {
  const raw = Buffer.from(await c.req.arrayBuffer())
  const discord = discordChannel()
  try {
    await discord.verify(c.req.raw.headers, raw)
  } catch {
    return c.body(null, 401)
  }
  const body = JSON.parse(raw.toString()) as {
    type?: number
    data?: { name?: string; options?: { name?: string; value?: string }[] }
    user?: { id?: string }
    member?: { user?: { id?: string } }
  }
  if (body.type === 1) return c.json({ type: 1 })

  if (body.type === 2 && body.data?.name === "link") {
    const code = body.data.options?.find((option) => option.name === "code")?.value ?? ""
    const userId = body.member?.user?.id ?? body.user?.id ?? ""
    const ok = await redeemLink(sql, "discord", userId, code)
    return c.json({
      type: 4,
      data: {
        content: ok ? "Linked. Approval requests will arrive here." : "That code is expired or already used.",
        flags: 64,
      },
    })
  }

  const inbound = discord.parse(body)
  if (inbound) {
    const { waitUntil } = await import("@vercel/functions")
    waitUntil((async () => {
      await sweep(sql, channels())
      await decide(sql, { ...inbound, channelName: "discord" }, channels())
      await deliverDue(sql)
    })())
  }
  return c.json({ type: 6 })
})

app.get("/webhooks/whatsapp", (c) => {
  const mode = c.req.query("hub.mode")
  const token = c.req.query("hub.verify_token")
  const challenge = c.req.query("hub.challenge") ?? ""
  if (mode === "subscribe" && token === process.env.WHATSAPP_VERIFY_TOKEN) return c.text(challenge)
  return c.body(null, 403)
})

app.post("/webhooks/whatsapp", async (c) => {
  const raw = Buffer.from(await c.req.arrayBuffer())
  const whatsapp = whatsappChannel()
  try {
    await whatsapp.verify(c.req.raw.headers, raw)
  } catch {
    return c.body(null, 401)
  }
  const body = JSON.parse(raw.toString())
  const message = firstMessage(body)
  const text = message?.text?.body?.trim() ?? ""
  if (message?.from && message.type === "text" && /^\/link\s+\d{6}$/.test(text)) {
    const ok = await redeemLink(sql, "whatsapp", message.from, text.split(/\s+/)[1])
    await whatsapp.send(message.from, {
      title: ok ? "Linked" : "Not linked",
      body: ok ? "Approval requests will arrive here." : "That code is expired or already used.",
      fields: [],
      actions: [],
    })
    return c.body(null, 200)
  }
  const inbound = whatsapp.parse(body)
  if (inbound) {
    await sweep(sql, channels())
    await decide(sql, { ...inbound, channelName: "whatsapp" }, channels())
    await deliverDue(sql)
  }
  return c.body(null, 200)
})

app.get("/a/:token", async (c) => {
  await sweep(sql, channels())
  return c.html(await renderTokenPage(c.req.param("token")))
})

app.post("/a/:token", async (c) => {
  await sweep(sql, channels())
  const form = await c.req.parseBody()
  const action = form.action === "approve" || form.action === "reject" ? form.action : null
  if (!action) return c.html(pageHtml({ gone: true }), 400)
  await decide(sql, {
    token: c.req.param("token"),
    externalUserId: "",
    channelName: "whatsapp",
    action,
  }, channels())
  await deliverDue(sql)
  return c.html(await renderTokenPage(c.req.param("token")))
})

app.on(["GET", "POST"], "/internal/sweep", async (c) => {
  const expected = `Bearer ${process.env.CRON_SECRET ?? ""}`
  if (c.req.header("authorization") !== expected || !process.env.CRON_SECRET) {
    return c.body(null, 401)
  }
  await sweep(sql, channels())
  return c.json({ ok: true })
})

async function renderTokenPage(token: string) {
  const row = await previewOpenToken(sql, token)
  if (!row) return pageHtml({ gone: true })
  return pageHtml({
    title: row.title,
    body: row.body,
    fields: row.fields,
    status: row.status,
    buttons: row.status === "pending" && !row.used_at,
  })
}

function parseCreate(body: unknown) {
  if (!body || typeof body !== "object") return null
  const value = body as Record<string, unknown>
  const externalRef = text(value.external_ref, 200)
  const title = text(value.title, 200)
  const callbackUrl = httpsUrl(value.callback_url)
  const expiresIn = Math.floor(Number(value.expires_in))
  if (!externalRef || !title || !callbackUrl) return null
  if (!Number.isFinite(expiresIn) || expiresIn < 1 || expiresIn > 60 * 60 * 24 * 366) return null
  const fields = Array.isArray(value.fields)
    ? value.fields.flatMap((field) => {
        if (!field || typeof field !== "object") return []
        const item = field as Record<string, unknown>
        const label = text(item.label, 80)
        const fieldValue = text(item.value, 200)
        return label && fieldValue ? [{ label, value: fieldValue }] : []
      })
    : []
  const rule = typeof value.rule === "string" ? value.rule : ""
  const approvers = stringList(value.approvers)
  const steps = Array.isArray(value.steps)
    ? value.steps.flatMap((step) => {
        if (!step || typeof step !== "object") return []
        const item = step as Record<string, unknown>
        if (item.rule !== "any" && item.rule !== "all") return []
        return [{ rule: item.rule, approvers: stringList(item.approvers) }]
      })
    : undefined
  return {
    externalRef,
    title,
    body: text(value.body, 4000) ?? "",
    fields,
    rule,
    approvers,
    steps,
    callbackUrl,
    expiresIn,
  }
}

function text(value: unknown, max: number): string | null {
  if (typeof value !== "string") return null
  const trimmed = value.trim()
  if (!trimmed || trimmed.length > max) return null
  return trimmed
}

function stringList(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  return value.filter((item): item is string => typeof item === "string" && item.length > 0 && item.length <= 80)
}

function httpsUrl(value: unknown): string | null {
  if (typeof value !== "string") return null
  try {
    const url = new URL(value)
    if (url.protocol !== "https:") return null
    return url.toString()
  } catch {
    return null
  }
}

export default app
```

Discord returns `{ type: 6 }` before any database work on a button tap, then finishes the decision with `waitUntil`. That is what keeps the handler inside Discord's 3 seconds when Neon is waking up. Signature verification uses the raw body and does not touch the database.

`/link` on Discord is interaction type `2`. The reply is type `4`. Button taps are type `3`. A ping is type `1`, and the response is `{ type: 1 }`.

## 8. Run it locally

```powershell
npx vercel dev
```

Leave this command out of the `dev` script. `vercel dev` runs that script, so a script that calls `vercel dev` starts itself again.

Create a request with the seeded key:

```powershell
curl.exe --% -s -X POST http://localhost:3000/v1/requests -H "Authorization: Bearer dev-key" -H "Content-Type: application/json" -d "{\"external_ref\":\"deploy-1\",\"title\":\"Deploy billing-api\",\"body\":\"sha 9f3c1a by ada\",\"fields\":[{\"label\":\"Env\",\"value\":\"prod\"}],\"rule\":\"any\",\"approvers\":[\"ada\",\"bob\"],\"callback_url\":\"https://example.com/hook\",\"expires_in\":3600}"
```

`--%` tells PowerShell to stop editing the rest of the line. Without it, PowerShell strips the quotes inside the JSON, and the server receives text that is not JSON.

The response is `{ "id": "req_…", "status": "pending" }`. Ada has no chat linked yet, so no card is sent. The same `external_ref` again returns the same id.

```powershell
curl.exe --% -s -X POST http://localhost:3000/v1/link-codes -H "Authorization: Bearer dev-key" -H "Content-Type: application/json" -d "{\"user\":\"ada\"}"
```

The response is `{ "code": "482913" }`. Ada sends `/link 482913` to the bot after the webhook is registered. Then create a new `external_ref`. The card goes to the linked chat.

Read and cancel:

```powershell
curl.exe -s http://localhost:3000/v1/requests/req_ID -H "Authorization: Bearer dev-key"
curl.exe -s -X POST http://localhost:3000/v1/requests/req_ID/cancel -H "Authorization: Bearer dev-key"
```

A second cancel returns the same terminal request and does not change it.

## 9. Deploy and point the bots

```powershell
npx vercel
```

Put every `.env` value into the Vercel project, then deploy again so the functions see them. `CRON_SECRET` is what Vercel sends as `Authorization: Bearer …` on the daily cron. The sweep route rejects any other caller.

Use the production URL below as `https://YOUR-HOST`.

Telegram, from a browser or curl, with the bot token and the same secret as `TELEGRAM_WEBHOOK_SECRET`:

```text
https://api.telegram.org/bot<token>/setWebhook?url=https://YOUR-HOST/webhooks/telegram&secret_token=<TELEGRAM_WEBHOOK_SECRET>
```

Discord, in the developer portal: paste the public key into `DISCORD_PUBLIC_KEY`, create a bot, and set the interactions endpoint to `https://YOUR-HOST/webhooks/discord`. Discord sends a ping. The route answers `{ type: 1 }` with no database call. Invite the bot to a server the approver is in.

Register the link command once:

```powershell
curl.exe --% -s -X POST https://discord.com/api/v10/applications/APP_ID/commands -H "Authorization: Bot BOT_TOKEN" -H "Content-Type: application/json" -d "{\"name\":\"link\",\"description\":\"Link this account to Pass\",\"options\":[{\"name\":\"code\",\"description\":\"6 digit code\",\"type\":3,\"required\":true}]}"
```

WhatsApp, in the Meta app: callback URL `https://YOUR-HOST/webhooks/whatsapp`, verify token equal to `WHATSAPP_VERIFY_TOKEN`, and a subscribed `messages` field. Create the utility template from step 6 with the URL `https://YOUR-HOST/a/{{1}}`. Put its name in `WHATSAPP_TEMPLATE`.

Approvers link themselves, then the calling app creates requests. The signed callback posts to `callback_url` when the request leaves `pending`. The caller rejects a timestamp older than 5 minutes, checks the HMAC over `timestamp + "." + raw body` with the tenant callback secret, and treats a repeat of the same `id` and `status` as the same event.

On this free deploy, a tenant that sends no traffic can stay `pending` until the daily sweep, up to about a day past `expires_in`. A free pinger can call `POST /internal/sweep` with `Authorization: Bearer <CRON_SECRET>` every few minutes. The function is the same one the API already runs.

## Ceilings

- Card sends are not retried. A failed send leaves the request `pending` until someone creates a new `external_ref` or the approver is linked and a later step notifies them.
- WhatsApp cannot edit the original interactive message. The outcome is a second text message, and outside 24 hours that text may not be delivered. The page is the record.
- Discord DMs require a shared server. The link step is the `/link` slash command.
- Telegram delivers cards only after the approver has started the bot.
- Callback retries wait 1, 5, 15, 60, 180, 360, then 720 minutes, and then the outbox row is dead.
