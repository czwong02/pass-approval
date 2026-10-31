import assert from "node:assert/strict"
import test, { after } from "node:test"
import { MemoryChannel } from "./channels/memory.ts"
import { sql } from "./db.ts"
import { createRequest, decide, getRequest } from "./core/store.ts"
import { newId } from "./core/tokens.ts"

after(() => sql.end())

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
    channels: { test: memory },
    async cleanup() {
      await sql`delete from tenants where id = ${tenantId}`
    },
  }
}
