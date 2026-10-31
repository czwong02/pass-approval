import { createHmac } from "node:crypto"
import type { Db, Query } from "../db.ts"
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

async function loadRequest(db: Query, id: string): Promise<RequestRow> {
  const rows = await db<RequestRow[]>`select * from requests where id = ${id}`
  if (!rows[0]) throw new Error("missing request")
  return rows[0]
}

async function present(db: Query, request: RequestRow) {
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

async function enqueue(db: Query, request: RequestRow) {
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