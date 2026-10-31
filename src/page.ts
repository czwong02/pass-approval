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
