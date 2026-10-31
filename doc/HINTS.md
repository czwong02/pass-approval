# Hints

Build from [DESIGN.md](DESIGN.md) and [PRD.md](PRD.md). This file only says what to figure out. [TUTORIAL.md](TUTORIAL.md) is the answer key; open a step there after yours already runs.

The project setup is already in the repo. `src/db.ts` is empty, and there is no other source yet. Start at the database.

A step is done when the check at the bottom of it passes. Stay on that step until it does.

## Database

The design names the tables and the unique keys. Write one SQL file and run it in the Neon SQL Editor against the same database as `DATABASE_URL`.

One column is missing from the design's `action_tokens` table. A tap has to know which step it belongs to, and a decision is unique per request, step, and approver. Add the column that makes that check possible.

Before any request can exist you need a tenant and some users. The API stores a hash of the key, so decide which hash you will compare when a request arrives, and put that hash in the tenant row. Pick a callback secret and store it beside the hash.

Done when the Neon sidebar lists every table from the design, plus a tenant and at least two users.

## Database client

`src/db.ts` creates the one client every other file will import.

The URL is the pooled one. Three facts about that host decide the options you pass:

- Neon refuses an unencrypted connection.
- A serverless instance should not open a pool of connections.
- The pooler does not keep session state. One postgres.js default stores state on the session and will break a later query. Find that option in the postgres.js docs.

Export the client and a type for it. The type has to accept both the client and the handle you get inside a transaction, because the decision path uses both.

Done when a throwaway script can select a constant and print it.

## Rules

Put the rules in a file that imports nothing from the database or from HTTP. The PRD table is the whole spec.

Answer these before writing the conditions:

- On `any`, `all`, and `sequence`, which single action ends the request as rejected?
- For `all`, what is still true after the first approval?
- For `sequence`, who is allowed to be notified while step 1 is open?

You want two functions: one that returns `pending`, `approved`, or `rejected`, and one that returns the index of the step that is open, or nothing when the request is no longer pending. A sequence step is itself `any` or `all`, so the step check should be the same function the top rule uses.

Also decide how a create-body becomes that step list. `any` and `all` send approvers. `sequence` sends steps. Invalid shapes should fail before a row is inserted.

Done when you can call those functions on a few hand-written decision lists and match the PRD table, with no database running.

## Tokens

A button carries a token, and the database stores a hash of it. Decide how many random bytes make 128 bits, and which encoding keeps the token inside Telegram's 64-byte callback limit. The same hash function you used for the API key can hash the token and the link code.

Done when you can make a token, hash it, and the raw token is short enough for a Telegram button.

## Channel shape

Put the `Card`, `Inbound`, and `Channel` types in one file that the store and the adapters both import. Copy the four methods from the design.

One method cannot stay synchronous. Which provider's verification is async, and what does that do to the signature?

`Card` needs one extra field the design's type does not show: the raw token the WhatsApp fallback link will carry. Telegram and Discord never read it. Decide why that token cannot be one of the two button tokens. The page has to offer both actions, and you only store hashes.

Done when a fake channel can implement the interface and compile.

## Store

This is the large file. Build it in the order below. Each part calls the previous one.

**Create.** The idempotency key is `(tenant, external_ref)`. On a repeat, return the existing row and do not send. Reject approver ids that are not users of that tenant. `expires_in` is seconds from now.

**Notify.** Only the open step is notified. An approver with no linked chat gets nothing, and the request still exists. For each linked chat, create the approve token, the reject token, and the fallback token, store the hashes, send the card, and save the message id so you can edit it later. A failed send must not delete the request. Decide what you are willing to lose when a send fails, and leave a comment that names the upgrade.

**Decide.** One transaction, and lock the token row first. Then, in order: missing or already used, request not pending or past `expires_at`, the chat account is not linked to the approver on the token, the token's step is not the open step. A fallback token skips the link check, because holding it is the credential, and the action comes from the page. Insert the decision. The unique key makes the second insert do nothing. Update the request status in that same transaction, and only from `pending`. If the new status is terminal, write the outbox row in that same transaction.

**Edit.** You cannot put the old buttons back, because you stored hashes. While the request is pending, rewrite only the cards of people who have already voted, with no buttons. When the request leaves `pending`, rewrite every card with the outcome and no buttons. A second tap on a finished request should still reach that rewrite.

**Advance.** After a non-terminal decision, if the open step index changed, notify the new step. That is what keeps a sequence from messaging step 2 early.

**Link.** A 6-digit code, stored as a hash, valid for 10 minutes, single use. Redeeming it stores `(user, channel, external user id)`. The same chat account linking again replaces the previous user.

**Sweep.** One update expires every pending row whose deadline has passed, then each of those rows goes through the same outbox and card rewrite as a decision. Then send a small batch of due callbacks. Sign `timestamp + "." + raw body` with the tenant callback secret. On failure, delay and try again for about a day, then mark the row dead. Pick the delay list yourself.

Done when the checks in the next section pass. Do not start the HTTP layer first.

## Check

The check uses a channel that records `send` and `edit` in arrays. It never opens a socket. It creates its own tenant and deletes that tenant at the end, so point it at the Neon database and let the foreign keys cascade.

Cover these cases, one test each:

- `all` with A and B: A's approval leaves the request pending, the same token a second time inserts one decision, B's approval makes it approved.
- A sequence of two `any` steps sends a card only to step 1, and step 2 appears only after step 1 passes.
- A reject on `any` finishes the request as rejected with one decision.
- Creating the same `external_ref` again does not send another card.
- After you remove the chat link, a tap with a real token inserts nothing.

The check script is already in `package.json`. It reads `.env`.

Done when that command exits with five passes.

## Adapters

One file per provider. Each file implements `Channel` and nothing about rules.

**Telegram.** The webhook header is a shared secret, compared in constant time. A tap is a callback query; the token is the whole callback data. Answer the callback query before you do the decision work, or the button spinner hangs. Plain text `/link` plus the code is how an account links. The person has to start the bot once, or the send fails.

**Discord.** Verify the Ed25519 signature on the raw body before you parse and before you touch the database. A button tap must be acknowledged immediately with a deferred update; do the decision after that acknowledgement is scheduled to finish. The interactions URL does not receive ordinary chat text, so the link step has to be a slash command. A DM only works when the bot and the person share a server.

**WhatsApp.** Verify `X-Hub-Signature-256` over the raw body. A button id is the token. There is a GET challenge you must echo before Meta will deliver POSTs. Inside the 24-hour window, send an interactive message. When the send fails because that window is closed, send the template whose URL ends in the fallback token. The Cloud API cannot edit an interactive message; decide what `edit` can honestly do, and name that ceiling in a comment.

Done when each function returns the `Channel` shape and a bad signature throws.

## HTTP

One Hono app, exported as the default. Routes from the design:

- create, read, cancel, link codes, under a bearer key
- one webhook per provider
- the fallback page
- the sweep route

Run the sweep at the start of any route that reads or writes a request, so an active tenant expires on time. The daily cron is the backstop for a quiet tenant. The sweep route should refuse any caller that does not present `CRON_SECRET`. Vercel calls it with GET. A pinger may use POST. Accept both.

Read the webhook body once, as bytes, and hand those bytes to `verify`. Parsing first breaks Discord and WhatsApp signatures.

The page reads the fallback token, shows the card, and posts the chosen action back to the same decide path. Escape every value the caller supplied.

Validate the create body at the route: required strings, an https callback URL, a positive `expires_in`, and a rule the rules module will accept. Unknown approvers are a 400. A bad key is a 401. An unknown id for this tenant is a 404.

Done when the local curl from the tutorial's "run it" step returns an id, the same `external_ref` returns that same id, and a link code comes back for a seeded user.

## Deploy

Deploy only after the check passes. Put the same variables into the Vercel project. Then point each provider at the host:

- Telegram webhook URL plus the secret header value
- Discord interactions endpoint, then register the link command
- WhatsApp callback URL, the verify token, and a template whose button URL is the fallback page plus the token

Done when each provider's setup screen shows the endpoint as verified, and a linked user receives a card for a new `external_ref`.
