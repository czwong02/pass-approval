# Go hints

Build Pass from [PRD.md](PRD.md) and [DESIGN.md](DESIGN.md). A caller opens one approval request. The people who must decide get a card in Telegram, Discord, or WhatsApp. They tap Approve or Reject. When the rule is satisfied, Pass posts the result back to the caller.

This file says what to figure out in each step. It does not depend on any other implementation.

`go.mod` is already at the repo root. The Neon database already has the tables and a dev tenant. The API key for that tenant is `dev-key`. Do not create a second database.

Do the steps in order. A step is done when the check at the bottom of it passes. Stay on that step until it does.

1. Connect to Neon
2. Rules
3. Tokens
4. Channel
5. Store
6. HTTP
7. Telegram
8. Discord
9. WhatsApp
10. Deploy

## Step 1. Connect to Neon

The app talks to Postgres through the pooled URL in `.env`. That pooler does not keep a session between queries. A prepared statement is session state, so the second query can land on another session and fail. Find the driver setting that sends each query in the simple protocol instead.

The standard library has no Postgres driver. The first import outside the standard library is that driver. `go mod tidy` writes `go.sum` only after the import exists. Until then, tidy has nothing to do.

Done when a small program prints one row from `select 1` and exits. Remove that program when the real server exists. The host looks for one server at the root of the module.

## Step 2. Rules

Put the rules in a package that does not import HTTP or a database. One request has one rule. Reject is a veto on every rule.

| Rule | Approved when | Rejected when |
| --- | --- | --- |
| `any` | The first approver approves. | Any approver rejects. |
| `all` | Every approver approves. | Any approver rejects. |
| `sequence` | Each step passes, in order. A step is itself `any` or `all`. | The open step is rejected. |

An unanswered request expires separately. The rule functions do not need a clock. They only answer: given the steps and the decisions so far, is this pending, approved, or rejected?

You need two results from that package. One is the status. The other is the open step, or nothing when the request is already finished. Only people on the open step may be notified or may tap. A sequence does not tell step 2 anything while step 1 is still open.

A create body uses approvers for `any` and `all`. It uses an ordered list of steps for `sequence`. An invalid shape fails before any row is inserted.

Done when a test feeds hand-written decision lists into those functions and matches the table above. The test does not open a database.

## Step 3. Tokens

Each button carries its own token. The database stores only a hash, so a copy of the table is not enough to tap the button. Use the same hash for the API key, the link code, and the action token.

The token is 128 random bits. Telegram allows 64 bytes in the button payload, so the encoding has to stay under that. Discord and WhatsApp button ids can use the same string.

A link code is 6 digits and lasts 10 minutes. A request id starts with `req_` so you can tell it from other ids in the database.

Done when a test shows a fresh token is short enough for a Telegram button, and hashing the same string twice gives the same digest.

## Step 4. Channel

Telegram, Discord, and WhatsApp all show the same card: a title, a body, label/value fields, and while the request is pending, Approve and Reject. After the request is finished, the card shows the outcome and has no buttons.

Describe that card once, and an interface with four operations: send a card to a person, edit a card you already sent, check that a webhook really came from the provider, and turn a webhook into "this person tapped this token." The rest of the app calls the interface. It does not import a chat SDK, and it does not write HTTP responses.

The WhatsApp fallback is a link, and a link can hold one token. The page at that link still has to offer both Approve and Reject. You cannot recover a button token later, because you stored the hash. So each person also gets a third token whose only job is to open that page. Chat buttons keep their own approve token and reject token.

Discord and WhatsApp sign the raw request bytes. Parsing JSON first can change those bytes, and the signature check then fails even for a real webhook. The verify operation has to see the original bytes and the headers.

Done when an in-memory implementation records every send and every edit, and the package compiles. The later tests use that implementation instead of a real chat.

## Step 5. Store

This package is the product. Build the parts in the order below. Each part calls the previous one. The step is done only when the five tests at the end pass.

**Create.** A caller sends an `external_ref` that is unique for their tenant, a title, a body, optional display fields, the rule and the approvers, an `https` callback URL, and `expires_in` in seconds. The deadline is now plus that many seconds. Approver ids must already be users of that tenant. Inserting the same `external_ref` again returns the existing request and does not send another card.

**Notify.** Only approvers on the open step get a card, and only on chats they have linked. No link means no card, and the request still exists. For each of those people, store three token hashes: approve, reject, and the page token. Then send. Save the provider's message id so a later decision can edit that card. If the send fails, keep the request. Leave a short comment that a failed send is not retried, and that the upgrade is a queue of cards to send, like the callback queue.

**Decide.** One database transaction. Lock the token row and the request row. Take those locks in the same order in every transaction, so two decisions cannot deadlock by locking them in opposite directions.

Then stop, in this order, when any of these is true: the token is missing or already used, the request is not pending or the deadline has passed, the chat account is not linked to the approver named on the token, or the token's step is not the open step. The page token is the exception. Whoever holds it may choose Approve or Reject, and you do not look up a chat account.

Insert the decision. The unique key is the request, the step, and the approver, so a second tap inserts nothing. Mark the token used. If the rule result is no longer pending, set the status only while it is still pending, and write the callback row in the same transaction.

**Edit.** You stored hashes, so you cannot rebuild the old buttons. While the request is still pending, rewrite only the cards of people who have already voted, and remove their buttons. Everyone else keeps the buttons they were sent. When the request leaves pending, rewrite every card with the outcome and no buttons.

**Advance.** After a decision that leaves the request pending, if the open step moved forward, notify that new step. That is what keeps a sequence from messaging step 2 early.

**Link.** The caller asks for a code for one user. You return 6 digits and store the hash, valid for 10 minutes, single use. The person sends that code to the bot. You store their user, the channel, and the chat account id. If that chat account links again, it replaces the previous user.

**Sweep.** One update marks every pending request past its deadline as expired. Each of those uses the same callback and card rewrite as a decision. Then send a small batch of due callbacks, about 10. The body is the request id, the caller's ref, the status, when it was decided, and the decisions. Sign `timestamp + "." + raw body` with the tenant's callback secret. The headers are `X-Pass-Timestamp` and `X-Pass-Signature`. On failure, wait and try again until about a day has passed, then mark that row dead. Pick the waits yourself. Write them in one list.

Run `go mod tidy` once this package imports the driver.

Done when `go test` passes these five cases. Each test creates its own tenant in Neon and deletes it at the end. The channel is the in-memory one. No test opens a socket to a chat provider.

1. Rule `all`, approvers A and B. A's approval leaves it pending. The same token again inserts one decision. B's approval makes it approved.
2. A sequence of two `any` steps sends a card only to step 1. Step 2 is sent after step 1 passes, and the request is still pending.
3. A reject on `any` finishes the request as rejected with one decision.
4. Creating the same `external_ref` again does not send another card.
5. After you remove the chat link, a tap with a real token inserts nothing.

## Step 6. HTTP

One process. Listen on `PORT` when the host sets it. When `PORT` is empty, listen on a fixed local port so you can run it on your machine.

These are the routes:

- `POST /v1/requests` creates a request. The caller sends `Authorization: Bearer <api key>`. You store and compare the hash of that key.
- `GET /v1/requests/:id` returns the request and its decisions.
- `POST /v1/requests/:id/cancel` cancels a pending request, rewrites its cards, and queues the callback. Cancelling a finished request changes nothing and returns the request as it is.
- `POST /v1/link-codes` with `{ "user": "ada" }` returns `{ "code": "482913" }`.
- One webhook URL per chat.
- `GET /a/:token` and `POST /a/:token` are the fallback page.
- `GET /internal/sweep` and `POST /internal/sweep` run expiry and callback delivery. Refuse the call unless `Authorization` is `Bearer` plus `CRON_SECRET`.

Run the sweep at the start of any route that reads or writes a request, so an active caller expires overdue requests as part of that call. The daily job is only the backstop for a quiet day.

Read a webhook body once, as bytes, and pass those bytes to verify.

Check the create body before the store sees it: required text within a length limit, an `https` callback, and `expires_in` from 1 second through 366 days. A bad body is 400. An unknown approver is 400. A bad key is 401. An id this tenant does not own is 404.

The page escapes the title, the body, and the fields. Those strings came from the caller. The standard library has a package that escapes HTML.

Done when a create from your machine returns an id and status `pending`, and the same `external_ref` returns that same id. In PowerShell, put `--%` immediately after `curl.exe`, or the shell will strip the quotes out of the JSON and the server will answer `bad request`.

## Step 7. Telegram

One bot, one webhook. The provider sends a shared secret in `X-Telegram-Bot-Api-Secret-Token`. Compare it in constant time. A mismatch is 401 and is not a decision.

A tap arrives as a callback query. The token is the entire callback payload. Answer that query before you record the decision, or the button keeps spinning. `/link` followed by the 6-digit code is an ordinary text message, not a button tap.

The person must open the bot and tap Start once. Until they do, the send fails. Keep the request anyway, as step 5 described.

Done when a wrong secret is rejected and no decision row is written.

## Step 8. Discord

Verify the Ed25519 signature on the raw body before you parse and before you touch the database. The signature header is `X-Signature-Ed25519`. The timestamp header is `X-Signature-Timestamp`. Discord signed the timestamp bytes followed by the raw body, not the body alone. The public key is hex. The standard library can verify that.

A ping is interaction type 1. Reply `{ "type": 1 }` and do not open the database. Discord sends this ping when you save the webhook URL.

A button is type 3. The token is the button's custom id. Reply `{ "type": 6 }` before the decision work. Discord must see that reply within 3 seconds. The decision can finish after the reply is queued.

`/link` cannot be a plain DM. This webhook only receives interactions. Make `/link` a slash command with one required string option, the code. That interaction is type 2. Reply with type 4, a short message saying whether the code worked.

Send the card as a direct message. A bot can DM someone who shares a server with it and who allows DMs from server members. When you send, keep the DM channel id next to the message id. Editing then patches that message. You should not need to open the DM again just to learn the channel id.

Done when a ping returns without a database query, and a bad signature writes nothing.

## Step 9. WhatsApp

Verify `X-Hub-Signature-256` over the raw body with the app secret. A mismatch is 401.

Before Meta will POST messages, it sends a GET with `hub.mode`, `hub.verify_token`, and `hub.challenge`. When the verify token matches yours, the response body is the challenge, as plain text.

A button tap puts the token in the button id. `/link` plus the code is a text message.

Inside 24 hours of that person last writing to the business number, send an interactive message with two reply buttons. Outside that window the send fails with error 131047. Then send the pre-approved template. The template has one URL button, and the URL ends with the page token from step 4.

WhatsApp will not edit an interactive message. A follow-up text with the outcome is as far as edit can go. Outside the 24-hour window that text may also fail. The page is where the person still sees the real status. Write that limit in a comment next to edit.

Done when the GET challenge echoes the value Meta sent, and a bad signature is rejected.

## Step 10. Deploy

Deploy only after `go test` passes. The host is Vercel, Go preset. The process listens on `PORT`. The cron calls `GET /internal/sweep` once a day. A schedule that runs more often will fail on the free plan.

Set the same variables the app already reads: `DATABASE_URL`, `CRON_SECRET`, the Telegram bot token and webhook secret, the Discord public key, bot token, and application id, and the WhatsApp token, app secret, phone number id, verify token, and template name.

Save the Discord interactions URL first. Discord sends the ping from step 8. After that ping succeeds, register the `/link` slash command. Then point Telegram and WhatsApp at their webhook URLs.

Done when one real request is created, someone linked to a chat taps it, and the signed callback arrives at the caller's URL.
