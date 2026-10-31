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
