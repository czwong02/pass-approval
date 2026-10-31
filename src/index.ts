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
    if (error instanceof Error && (error.message === "unknown approver" || error.message === "bad request")) {
      return c.json({ error: error.message }, 400)
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
        return [{ rule: item.rule as "any" | "all", approvers: stringList(item.approvers) }]
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
