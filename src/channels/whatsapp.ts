import { createHmac, timingSafeEqual } from "node:crypto"
import { renderText, type Card, type Channel, type Inbound } from "../core/channel.ts"

const graph = "https://graph.facebook.com/v22.0"

export function whatsappChannel(): Channel {
  return {
    async verify(headers, rawBody) {
      const expected = Buffer.from(
        "sha256=" + createHmac("sha256", process.env.WHATSAPP_APP_SECRET ?? "").update(rawBody).digest("hex"),
      )
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
    // and the fallback page is the record.
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
