# Pass Approval — Product requirements

Pass asks for a decision in the chat the approver already uses, then tells the calling app the result. v1 is an API. There is no approver website except the WhatsApp fallback page.

Technical contract: [DESIGN.md](DESIGN.md). Free hosting: [DEPLOY.md](DEPLOY.md).

## Problem

An app that needs a human yes or no usually builds its own inbox, login, and notification path. The approver then leaves the chat they are already in. Pass is that path for them: one request, a card with Approve and Reject, a signed callback when the request is finished.

## Users

| Who | What they do |
| --- | --- |
| Calling app | Creates a request, reads it, cancels it, and receives the result. Authenticates with a tenant API key. |
| Tenant | Owns users, the API key, and the callback secret. v1 has no admin UI. Tenants, users, and keys are rows in Postgres. |
| Approver | Links a chat account once, then taps Approve or Reject on a card. Does not get a Pass login. |

## Outcome

A calling app can open a request for named people, under `any`, `all`, or `sequence`. Each person who must act now gets the same card on Telegram, Discord, or WhatsApp. A tap from a linked approver on the open step counts once. When the rule passes, fails, expires, or is cancelled, Pass rewrites the open cards and posts one signed result to the caller.

## Request

A request is one question with a deadline.

The caller sends an `external_ref` that is unique for that tenant, a title, a body, optional display-only fields, the rule and the approvers, a `callback_url`, and `expires_in` in seconds.

Statuses move from `pending` to exactly one of `approved`, `rejected`, `expired`, or `cancelled`. A terminal status stays terminal.

Creating the same `external_ref` again returns the existing request and does not send another card.

The caller can read a request, including its decisions, and can cancel a `pending` request. Cancelling a terminal request changes nothing. A cancel rewrites the open cards.

## Rules

One request has one rule. Reject is a veto on every rule.

| Rule | The request is approved when | The request is rejected when |
| --- | --- | --- |
| `any` | The first approver approves. | Any approver rejects. |
| `all` | Every approver approves. | Any approver rejects. |
| `sequence` | Each step passes, in order. A step is itself `any` or `all`. | The open step is rejected. |

An unanswered request becomes `expired` once `expires_in` has passed, whichever rule it uses.

On a sequence, only the approvers of the open step get a card. The next step is notified after the current step passes.

## Who can decide

A tap counts only when all of these are true:

- The chat account is linked to an internal user.
- That user is an approver on this request.
- That user is on the step that is open.
- The action token is unused and the request is still `pending`.

The caller links a user by creating a 6-digit code, valid for 10 minutes. The approver sends `/link <code>` to the Telegram bot, the WhatsApp number, or the Discord bot. Pass stores the user, the channel, and the chat account id.

A tap from an unlinked account is ignored. A second tap by the same approver on the same step writes nothing new and updates the card to the current status.

## The card

Every channel shows the same card: title, body, fields, and while the request is `pending`, Approve and Reject. After the request leaves `pending`, the card shows the outcome and has no buttons.

Each button carries a single-use token bound to that request, approver, and action. The token expires with the request. The button does not carry the request payload.

WhatsApp can send a free-form card only inside the 24-hour window after that person last wrote to the business number. Outside that window, Pass sends a pre-approved template with one link. The link opens a page with the same card and the same two actions, and that page uses the same decision path. The token is still single-use.

Discord cards go out as direct messages, so the request is not posted in a channel other people can read.

## Callback

When a request leaves `pending`, Pass posts the result to `callback_url`. Delivery is at-least-once. The body is stable for that `id` and `status`. Retries send the same body with a new timestamp. The caller treats a repeat of the same `id` and `status` as the same event.

The post is signed with the tenant secret. The caller rejects a timestamp older than 5 minutes and rejects a signature that does not match.

Pass retries for 24 hours, then keeps one dead-letter row.

## Security

- Tenant routes require `Authorization: Bearer <api key>`. The stored key is a hash.
- Chat webhooks are verified before they are parsed. A failed verify is a 401 and is not a decision.
- Action tokens are stored as a hash.
- Link codes are stored as a hash.

## Acceptance

These are the behaviors v1 has to demonstrate. The rule check does not open a socket to a chat provider.

- A request with rule `all` and approvers A and B stays `pending` after A approves, and becomes `approved` after B approves.
- Presenting the same token twice inserts one decision.
- A `sequence` of two `any` steps sends no card for step 2 until step 1 passes.
- A reject on any rule moves the request to `rejected` and notifies the caller.
- A repeat `external_ref` returns the current request and does not send another card.
- A cancel of a `pending` request moves it to `cancelled`, rewrites open cards, and notifies the caller. A cancel of a terminal request is a no-op.
- An unlinked tap and a tap from someone who is not the approver on the open step do not insert a decision.
- A webhook that fails verification does not insert a decision.
- The callback signature covers `timestamp + "." + raw body`, and a retry keeps the body.

## Free-host ceiling

On the free deploy in [DEPLOY.md](DEPLOY.md), expiry and callback retry run when a request comes in and at least once a day. A tenant that sends no traffic can see an unanswered request stay `pending` until that daily sweep, up to about a day past `expires_in`. A minute-level sweep is an external ping to the same path, still at no charge, or a paid cron.

## Out of scope for v1

- A form builder, attachments, nested requests, and delegation.
- Slack, email, and a web inbox. Each later channel is another adapter. Email uses the same link as the WhatsApp fallback.
- An admin UI for tenants, users, and API keys.
- A guild Discord message. v1 sends direct messages.
- A cache in front of token lookup.
