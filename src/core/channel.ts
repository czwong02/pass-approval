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
