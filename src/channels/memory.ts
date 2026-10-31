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
