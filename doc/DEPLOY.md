# Deploy

Use Vercel for the app and Neon for Postgres. Render’s free web service sleeps, and this design cannot survive that sleep.

## Why those two

Pass is a public HTTPS API plus three chat webhooks, a one-tap page, and Postgres. The worker in [DESIGN.md](DESIGN.md) only expires requests and retries callbacks. It does not need its own process.

| Need | Vercel Hobby | Render free |
| --- | --- | --- |
| Webhook URL that answers while idle | Stays up. A cold start is usually under a second. | Spins down after 15 minutes of no traffic and takes about a minute to wake. Discord must get a response in 3 seconds, so a sleeping service fails the interaction and Discord disables the endpoint. |
| Postgres that keeps your data | Neon’s free plan does not expire. 1 GB per project. | Render’s free database is deleted 30 days after you create it. |
| Once-a-minute expiry | Hobby cron runs once a day. See the sweep below. | No free background worker. |

Vercel Hobby is for personal, non-commercial projects. If you charge for Pass, that plan is the wrong place for it.

## The one design change

Do not run `worker/` as a process. Make expiry and callback retry a sweep that runs inside the request path, and call the same function from a daily cron as a backstop.

On every `POST /v1/requests`, `GET /v1/requests/:id`, and inbound webhook, before the real work:

1. `UPDATE requests SET status = 'expired' WHERE status = 'pending' AND expires_at < now()` and enqueue those callbacks.
2. Send a small batch of due `callback_outbox` rows (about 10).

While anyone is using the API, expiry stays close to the deadline stored on the request. The daily cron only matters when the service is completely quiet. A request can sit up to a day past `expires_at` in that case. To get closer to once a minute without paying, point a free pinger such as [cron-job.org](https://cron-job.org) at the sweep URL.

## Deploy

1. Create a Neon project in the same region you will deploy on Vercel. US East is the usual choice. Copy the **pooled** connection string. Serverless opens a connection per request, and the direct string will exhaust Neon’s connection limit.
2. Put the app on Vercel as one Node service. The layout in [DESIGN.md](DESIGN.md) still holds: `api/`, `core/`, `channels/`. `worker/` becomes a route, `POST /internal/sweep`, that refuses any request whose `Authorization` header is not `Bearer <CRON_SECRET>`.
3. Set these environment variables on the Vercel project:
   - `DATABASE_URL`
   - `CRON_SECRET`
   - `TELEGRAM_BOT_TOKEN`
   - `TELEGRAM_WEBHOOK_SECRET`
   - `DISCORD_PUBLIC_KEY`
   - `DISCORD_BOT_TOKEN`
   - WhatsApp app secret and token
4. Add a daily cron in `vercel.json`. Hobby rejects any schedule that runs more than once a day.

```json
{
  "crons": [
    { "path": "/internal/sweep", "schedule": "0 8 * * *" }
  ]
}
```

Vercel sends `Authorization: Bearer <CRON_SECRET>` on those calls once `CRON_SECRET` is set on the project. The sweep runs sometime during that hour, not at 08:00 exactly.

5. Deploy with `npx vercel`. Then point the bots at `https://<project>.vercel.app`:
   - Telegram: `setWebhook` to `/webhooks/telegram` with the secret token.
   - Discord: interactions endpoint URL to `/webhooks/discord`. Signature verification is local Ed25519 and does not touch the database, so acknowledge the interaction before the Neon query. That keeps the handler inside Discord’s 3 seconds when the database is scaled to zero.
   - WhatsApp: callback URL to `/webhooks/whatsapp`. The template button URL is `https://<project>.vercel.app/a/<token>`.

Neon compute sleeps after 5 minutes. The first tap after a quiet period pays that wake. Verify the Discord signature and return the deferred acknowledgement before opening a query, and that wake stays off the 3-second clock.
