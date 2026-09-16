# PaperBoxd Plus

Auto-renewing subscription. ₹199/month, ₹1,499/year, 7-day free trial (set
up as an introductory offer / base-plan offer in the stores — the code reads
it, never assumes it). Sold only through Apple IAP and Google Play Billing —
the only payment paths either store allows for an in-app digital feature in
India. No Razorpay, no web checkout. The web reads the entitlement and shows
what it unlocks, but sends readers to the app to subscribe.

## What Plus gates (2026-09-17)

| Feature | Free | Plus | Enforced |
|---|---|---|---|
| Ask Jazy (`POST /jazy`) | — | yes | backend 402 |
| Scan & Know | 3 / calendar month (`users.scan_uses_remaining`, `scan_period`) | unlimited | backend 403 `scans_exhausted` |
| Smart TBR (`GET /users/me/tbr/smart`) | — | yes | backend 402 |
| Taste dashboard (`GET /users/me/taste`) | top genres/authors, books rated | bars, shifts, mood, insights, dislikes | backend trims, `plus: false` |
| Taste twins (`GET /recommendations/twins`) | — | yes | backend 402 |
| Vibe search on the site | free | free | — (mobile has no vibe search outside Jazy) |
| Wrapped | free | free | — |

`GET /subscriptions/me` returns `entitlements` (`jazy`, `scan_unlimited`,
`vibe_search`, `reading_insights`, `taste_twins`) plus `scans_remaining` /
`scans_unlimited`; all keys follow the one subscription today.

## Analytics

Server: `subscription_started`, `trial_started`, `subscription_renewed`,
`subscription_cancelled`, `subscription_expired` (from link endpoints and
webhooks, metadata `store`, `product_id`, `trial`). Clients:
`paywall_viewed`, `premium_cta_clicked`, `premium_feature_viewed` with
`metadata.feature` (`jazy` / `scan` / `taste` / `smart_tbr`) and
`metadata.plan`.

## How it works

- `subscriptions` table, one row per user. Entitlement = `expires_at > now()`.
- Clients POST store receipts; the backend verifies them itself:
  - Apple: StoreKit 2 JWS, chain verified against Apple Root CA G3 locally
    (`internal/external/appstore.go`). No App Store Connect API key needed.
  - Google: purchase token looked up via Play Developer API with a service
    account, then acknowledged (`internal/external/playstore.go`).
- Store webhooks keep `expires_at` current on renew / cancel / refund:
  - Apple → `POST /api/v1/webhooks/apple` (Server Notifications V2, signed)
  - Google → `POST /api/v1/webhooks/google?token=…` (RTDN via Pub/Sub push)
- `/jazy` requires auth and returns `402 SUBSCRIPTION_REQUIRED` without an
  active row. Clients open the paywall on 402.

## Endpoints

| Method | Path | Auth | Body |
|---|---|---|---|
| GET | `/api/v1/subscriptions/me` | Bearer | — → `{active, store, product_id, expires_at, auto_renew}` |
| POST | `/api/v1/subscriptions/apple` | Bearer | `{"jws": "<Transaction.jwsRepresentation>"}` |
| POST | `/api/v1/subscriptions/google` | Bearer | `{"purchase_token": "…"}` |
| POST | `/api/v1/webhooks/apple` | JWS signature | Apple's `{signedPayload}` |
| POST | `/api/v1/webhooks/google` | `?token=` | Pub/Sub push envelope |

409 on a link means the receipt already backs another account.

## Env

```
GOOGLE_PLAY_PACKAGE=in.paperboxd.app                 # default
GOOGLE_PLAY_SERVICE_ACCOUNT_JSON='{ ...whole JSON key... }'
GOOGLE_PLAY_RTDN_TOKEN=<random secret>
APPLE_ALLOWED_AUDIENCES=com.paperboxd.PaperBoxd     # bundle id, already set
```

Apple needs nothing else.

## One-time setup

**App Store Connect**
1. App › Subscriptions › create group "Plus". Products:
   `in.paperboxd.plus.monthly` (1 month, ₹199), `in.paperboxd.plus.yearly` (1 year, ₹1,499).
   Localised name/description, review screenshot. On each: Introductory
   Offer › Free trial › 1 week (the paywall shows the trial only when
   StoreKit reports eligibility).
2. App Information › App Store Server Notifications › Production URL and
   Sandbox URL both = `https://<backend>/api/v1/webhooks/apple`, Version 2.
3. Agreements › Paid Apps agreement signed, bank + tax forms done.
4. App metadata: privacy policy URL + Terms (EULA) link — Guideline 3.1.2.

**Google Play Console**
1. Monetise › Subscriptions › product id `plus`. Base plans: `monthly`
   (P1M, ₹199), `yearly` (P1Y, ₹1,499). Activate both. On each base plan add
   an offer with a free-trial phase of P7D (any offer id — the app picks the
   offer with the longest zero-price phase and reads its length).
2. Google Cloud: enable "Google Play Android Developer API", create a service
   account, download its JSON key. Play Console › Users and permissions ›
   invite the service account email with "View financial data" + "Manage
   orders and subscriptions". Put the JSON in `GOOGLE_PLAY_SERVICE_ACCOUNT_JSON`.
3. Cloud Pub/Sub: topic `play-rtdn`, push subscription with endpoint
   `https://<backend>/api/v1/webhooks/google?token=<GOOGLE_PLAY_RTDN_TOKEN>`.
   Grant `google-play-developer-notifications@system.gserviceaccount.com`
   Pub/Sub Publisher on the topic. Play Console › Monetisation setup › paste
   the topic name, send test notification.
4. Upload a build with the billing dependency to any track before testing
   purchases; add tester accounts under Settings › Licence testing.

**Testing**
- iOS: sandbox Apple ID on device, or a `.storekit` config on the scheme.
  App Review buys in sandbox against the production backend — the backend
  accepts `environment: Sandbox` receipts on purpose.
- Android: licence-tester Google account on a build installed from Play
  (internal testing track).
