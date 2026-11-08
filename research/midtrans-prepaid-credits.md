# Midtrans Snap for prepaid-credit purchases

**Research date:** 2026-11-08
**Scope:** Official Midtrans developer documentation only. This report covers Snap checkout creation/display, amount representation, notification verification/processing, transaction status lifecycle, and status lookup/reconciliation for prepaid-credit purchases.

## Executive summary

Use **Snap hosted redirect** as the default checkout flow: create the transaction on the merchant backend using a unique server-generated order ID and authoritative purchase amount, then redirect the customer to Midtrans's returned `redirect_url`. Choose Snap Embedded instead only if keeping checkout within the app is a product requirement and the frontend can load Snap.js and host its embed container. In either mode, the backend owns transaction creation and fulfillment; a browser return/callback is not proof of payment.

Represent Midtrans Snap transaction amounts as **whole rupiah integers** (IDR, no decimal places); keep them distinct from the application's prepaid-credit quantity/ledger. Verify webhook `signature_key`, reconcile by fetching the current transaction status, and only grant credits once for an eligible, verified success. At minimum, Midtrans recommends success checks of `status_code == "200"`, `fraud_status == "accept"` (when present), and `transaction_status` of `capture` or `settlement`. For prepaid fulfillment, `settlement` is the conservative default; if fulfilling at `capture`, ensure later cancel/reversal/refund/chargeback events can compensate the credit ledger.

## Findings

### 1. Snap display choice: hosted redirect versus embedded

**Documented facts**

- Snap checkout starts with a merchant-backend request to `POST https://app.midtrans.com/snap/v1/transactions` (sandbox: `https://app.sandbox.midtrans.com/snap/v1/transactions`) authenticated with HTTP Basic using the **Server Key as username and an empty password**. A successful response provides a `token` and `redirect_url`. The Server Key must not be exposed to the browser. [Snap Integration Guide](https://docs.midtrans.com/docs/snap-snap-integration-guide)
- The frontend can redirect the customer to the returned hosted `redirect_url`; the guide describes this as useful when the merchant does not want to or cannot use Snap.js. Alternatively, Snap.js can embed checkout in a `<div>` with `window.snap.embed(token, { embedId })`. Embedded mode needs the Midtrans Snap.js script and a Client Key; the Client Key is distinct from the secret Server Key. [Snap Integration Guide](https://docs.midtrans.com/docs/snap-snap-integration-guide), [Snap JS reference](https://docs.midtrans.com/reference/snap-js)
- Midtrans says that a customer is redirected back after checkout and separately sends an HTTP notification to the merchant backend when transaction status changes. [Snap Integration Guide](https://docs.midtrans.com/docs/snap-snap-integration-guide)

**Recommendation (not a Midtrans requirement)**

- Prefer **hosted redirect** for the first prepaid-credit purchase flow: it minimizes frontend integration and keeps the payment UI hosted by Midtrans. Use the server-created `redirect_url`; do not construct it from customer input. The finish/return page should show a pending/processing state and fetch the merchant's own purchase status; do not grant credits based on redirect query parameters or Snap.js success callbacks.
- Use **embedded** if a seamless in-dashboard checkout is important enough to justify Snap.js loading, embed-container sizing/responsive behavior, and browser lifecycle handling. The same server-side notification and verification rules apply. Snap.js callbacks can improve UX, but must not be the source of truth.

### 2. Create transaction constraints and money/currency types

**Documented facts**

- `transaction_details.order_id` is required, unique per merchant transaction, at most 50 characters for Snap, and limited to alphanumeric characters plus dash (`-`), underscore (`_`), tilde (`~`), and dot (`.`). A used order ID cannot be reused for another order. [Transaction Details Object](https://docs.midtrans.com/reference/transaction-details-object), [Snap Integration Guide](https://docs.midtrans.com/docs/snap-snap-integration-guide)
- `transaction_details.gross_amount` is required, is an **integer** amount in **IDR**, and must not contain a decimal portion. The JSON object docs type it as an integer; the transaction-details reference specifies IDR and “Do not add decimal.” [Transaction Details Object](https://docs.midtrans.com/reference/transaction-details-object), [JSON Objects](https://docs.midtrans.com/reference/json-objects)
- If `item_details` are included, each item's price is an integer (no decimal), quantity is an integer at least 1, and the sum of item price × quantity must exactly equal `transaction_details.gross_amount`. Midtrans documents negative item prices as an option for representing a discount. [JSON Objects](https://docs.midtrans.com/reference/json-objects)
- Snap accepts `customer_details` and `item_details` as additional useful request fields. Snap API request creation should be managed securely by the merchant backend; Midtrans warns against making sensitive transaction parameters such as amount/order ID directly customer-editable. [Snap Integration Guide](https://docs.midtrans.com/docs/snap-snap-integration-guide), [Payment Security](https://docs.midtrans.com/docs/payment-security)
- Notification and GET Status examples show `gross_amount` as a string such as `"10000.00"` and examples include `currency: "IDR"`; that representation differs from the integer request field. [HTTP(S) Notifications](https://docs.midtrans.com/docs/https-notification-webhooks), [Get Transaction Status](https://docs.midtrans.com/reference/get-transaction-status)
- The reviewed Snap docs establish integer/IDR formatting and Long/integer field types, but do **not** specify a general Snap minimum or maximum amount. [Transaction Details Object](https://docs.midtrans.com/reference/transaction-details-object), [JSON Objects](https://docs.midtrans.com/reference/json-objects)

**Recommendation (not a Midtrans requirement)**

- Keep purchase price as an integer number of rupiah (or a fixed-precision money type that converts exactly to whole rupiah), and validate the conversion before requesting a Snap transaction. Do not use floating-point arithmetic for prices.
- Keep money amount and prepaid credit units as separate concepts. Persist a purchase snapshot (account/user, package or credit quantity, amount IDR, currency, order ID) before calling Midtrans, then send only the server-side snapshot amount. Ensure client-supplied package identifiers resolve to server-defined price/credit terms.
- Generate unique, bounded order IDs from internal purchase IDs and restrict characters to Midtrans's documented set. Persist order ID ↔ internal purchase mapping.
- If item details are supplied, ensure the line-item arithmetic exactly balances the gross amount, including fees/taxes/discounts represented as items. No explicit Midtrans-wide amount ceiling was found in these Snap references; confirm any account/payment-channel-specific bounds during onboarding and sandbox tests rather than assuming the integer type defines the business limit.

### 3. Notification authenticity and processing

**Documented facts**

- Midtrans sends HTTP(S) POST JSON notifications when the customer completes payment or transaction status changes. The notification endpoint is configured in Merchant Administration Portal (MAP); Midtrans requires the endpoint to be publicly reachable and recommends HTTPS. [HTTP(S) Notifications](https://docs.midtrans.com/docs/https-notification-webhooks)
- Verify the notification's `signature_key` by computing `SHA512(order_id + status_code + gross_amount + ServerKey)` and comparing it with the supplied signature. `gross_amount` participates as the notification's string value; preserve that exact representation when computing the signature. [Handle Notifications](https://docs.midtrans.com/reference/handle-notifications)
- Midtrans recommends idempotent notification handling, with `order_id` (or account ID) as a tracking key, because duplicate delivery can occur. It also recommends accepting future additional JSON fields rather than rejecting unknown fields. [Best Practices to Handle Notification](https://docs.midtrans.com/reference/best-practices-to-handle-notification)
- For a successful transaction Midtrans documents checking `status_code` = `200`, `fraud_status` = `accept` (when present), and transaction status `settlement` or `capture`. FDS `deny` means the transaction is rejected; `challenge` requires action and can later be automatically canceled if not approved. [Handle Notifications](https://docs.midtrans.com/reference/handle-notifications), [Transaction Status Cycle](https://docs.midtrans.com/docs/transaction-status-cycle)
- A 2xx response is treated as notification delivery success; the notification guidance recommends response time under five seconds. Notifications can be delayed or delivered out of order (e.g. settlement before pending); Midtrans says to use GET Status for the current status or ignore a stale pending notification. Retries are documented for delivery failures, with up to five retries (2m, 10m, 30m, 90m, 210m intervals, subject to random shifts); exact response-code policies and timeout figures vary slightly among current docs. [Best Practices to Handle Notification](https://docs.midtrans.com/reference/best-practices-to-handle-notification), [Handle Notifications](https://docs.midtrans.com/reference/handle-notifications), [HTTP(S) Notifications](https://docs.midtrans.com/docs/https-notification-webhooks)

**Recommendation (not a Midtrans requirement)**

1. Parse JSON permissively, locate the existing purchase by `order_id`, validate signature in constant time, and check expected merchant/account, amount, currency (when present), and purchase state before changing anything.
2. Treat notifications as signals to reconcile, not as an append-only sequence of truth. Fetch current status from GET Status where appropriate, especially for ambiguous/out-of-order events. Never assume every observed status is newer than the stored one.
3. Apply each status transition with a database transaction and idempotency key (purchase/order ID plus transition or a unique credit-ledger reference). Ensure duplicate notifications cannot grant the same credits twice.
4. Acknowledge promptly only after the notification has been durably accepted/queued. If processing is asynchronous, store the event and queue work before returning success; otherwise return a retryable error when persistence fails. This is an implementation recommendation, not a specified Midtrans queue contract.
5. Prefer granting prepaid credits on verified `settlement`. If the product chooses to grant at `capture`, the ledger must also handle later `cancel`, refund, chargeback, and reversal events. Do not grant for `pending`, `authorize`, `deny`, `cancel`, `expire`, or `failure` without an explicit separate business rule.

### 4. Status transitions, refunds, chargebacks, and reversal

**Documented facts**

- `pending` means awaiting customer payment and can move to `settlement`, `deny`, `cancel`, or `expire`. `capture` is successful captured card payment; it can move to `settlement` or `cancel`. `settlement` means funds received and can later move to `refund`, `partial_refund`, `chargeback`, or `partial_chargeback`. `deny`, `cancel`, `expire`, and `failure` are non-success outcomes. [Transaction Status Cycle](https://docs.midtrans.com/docs/transaction-status-cycle), [Transaction Status](https://docs.midtrans.com/reference/transaction-status)
- `authorize` is an advanced card pre-authorization state, not the default flow. It reserves a card balance; later it can become `capture`, `deny`, `cancel`, or `expire`. [Transaction Status Cycle](https://docs.midtrans.com/docs/transaction-status-cycle)
- A `partial_refund` can become `refund`; `partial_chargeback` can become `chargeback`. Chargeback is documented as applicable to payment cards. GET Status can include cumulative `refund_amount` and per-refund details. [Transaction Status](https://docs.midtrans.com/reference/transaction-status), [Get Transaction Status](https://docs.midtrans.com/reference/get-transaction-status)
- There is a documented rare **reversal** for Permata Bank Transfer, Mandiri Bill Payment, and Indomaret: status can change from `settlement` to `deny`, usually within 1–5 minutes. Midtrans says this means funds were reversed and the merchant should treat the transaction as not paid and not deliver the service. [Transaction Status Cycle](https://docs.midtrans.com/docs/transaction-status-cycle)
- For Snap specifically, creating a Snap transaction does not necessarily create a Core API status immediately: before the customer selects a payment method, GET Status may return 404/“Payment not found.” The status appears after they proceed with a payment method. [Transaction Status Cycle](https://docs.midtrans.com/docs/transaction-status-cycle)

**Recommendation (not a Midtrans requirement)**

- Model payments as a lifecycle, not a single boolean paid flag. Keep provider status and the internal credit-ledger outcome distinct, and allow later negative events to create compensating debit/hold entries rather than erasing prior ledger history.
- Consider `settlement` the safe activation point for prepaid credits, but still handle later refunds/chargebacks and the specified settlement→deny reversal. If credits have already been consumed, a policy is needed for negative balances, account holds, future top-ups, or write-off.
- Track partial refunds/chargebacks by amount using `refund_amount`/refund detail rather than treating any partial event as a full reversal. The mapping from money refunded to prepaid credit units is a business decision and should be deterministic and auditable.
- Treat the initial 404 after Snap creation as “not yet assigned/available,” not proof of failure; avoid creating a second order immediately. Retry status lookup later or await the notification, using a bounded reconciliation schedule.

### 5. GET Status and reconciliation

**Documented facts**

- The GET Status endpoint is `GET {BASE_URL}/v2/{order_id OR transaction_id}/status`, authenticated with Basic auth. Sandbox API base is `https://api.sandbox.midtrans.com`; production is `https://api.midtrans.com`. It supports Snap and Core API transactions. A transaction ID can replace order ID; for BI-SNAP and DANA payment methods, Midtrans says to use transaction ID only. [Get Transaction Status](https://docs.midtrans.com/reference/get-transaction-status), [Transaction Status Cycle](https://docs.midtrans.com/docs/transaction-status-cycle)
- The response includes `status_code`, `transaction_status`, `order_id`, `transaction_id`, `gross_amount`, and other payment details; refunds may include cumulative amount and details. A `404` response can mean no transaction record exists yet, including the Snap pre-payment-method case. [Get Transaction Status](https://docs.midtrans.com/reference/get-transaction-status), [Transaction Status Cycle](https://docs.midtrans.com/docs/transaction-status-cycle)
- Midtrans recommends GET Status when notifications are delayed or possibly out of order. [Best Practices to Handle Notification](https://docs.midtrans.com/reference/best-practices-to-handle-notification)

**Recommendation (not a Midtrans requirement)**

- Use GET Status to reconcile (a) notification signature/amount/status anomalies, (b) delayed or missing notifications, (c) ambiguous initial Snap 404, and (d) records periodically as an operational safety net. Midtrans publishes no polling interval/rate limit in the cited references; choose a bounded backoff and avoid aggressive polling.
- Compare fetched `order_id`, amount, currency where returned, and transaction ID with the saved purchase. Update state monotonically where possible, but allow explicitly documented later status changes such as `capture → cancel`, `settlement → refund/chargeback`, and the listed payment-method reversal `settlement → deny`.
- Save the last raw provider response (redacted as appropriate), status, observation time, and transaction ID for audit/reconciliation. A periodic job can enumerate locally open purchases and query status; an operator report should flag mismatched amounts, missing mappings, long-pending purchases, and negative status changes after credits were granted.

## Suggested implementation sequence

1. Server creates an immutable local purchase with a unique compatible `order_id`, account, package/credit amount, and whole-IDR price snapshot.
2. Server creates Snap transaction using that snapshot and secret Server Key; saves returned token and `redirect_url`; frontend redirects (or initializes embedded Snap with returned token).
3. Customer return is informational only. Backend receives notification, verifies signature and expected purchase fields, fetches current status if needed, and idempotently writes the provider transition and credit ledger entry.
4. A scheduled reconciliation process checks open/unconfirmed purchases and later settlement/refund/reversal/chargeback transitions through GET Status.

This is an integration recommendation based on Midtrans's documented flows, not a claim that Midtrans mandates a particular prepaid-credit ledger design.

## Primary sources

- [Snap Integration Guide](https://docs.midtrans.com/docs/snap-snap-integration-guide)
- [Snap JS reference](https://docs.midtrans.com/reference/snap-js)
- [Transaction Details Object](https://docs.midtrans.com/reference/transaction-details-object)
- [JSON Objects](https://docs.midtrans.com/reference/json-objects)
- [Payment Security](https://docs.midtrans.com/docs/payment-security)
- [HTTP(S) Notification / Webhooks](https://docs.midtrans.com/docs/https-notification-webhooks)
- [Handle Notifications](https://docs.midtrans.com/reference/handle-notifications)
- [Best Practices to Handle Notification](https://docs.midtrans.com/reference/best-practices-to-handle-notification)
- [Transaction Status Cycle](https://docs.midtrans.com/docs/transaction-status-cycle)
- [Transaction Status](https://docs.midtrans.com/reference/transaction-status)
- [Get Transaction Status](https://docs.midtrans.com/reference/get-transaction-status)
- [Handle After Payment](https://docs.midtrans.com/docs/handle-after-payment)
