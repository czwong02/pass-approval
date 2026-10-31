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
