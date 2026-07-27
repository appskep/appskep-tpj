# Create Order (v2)

How to create an order / start a payment in this backend. Covers the **v2** API only.

Source of truth:
- `routes/order.go` — route registration
- `handlers/v2/order.go` — `PostOrder`, `GetOrderURL`
- `handlers/v2/midtrans.go` — `PostMidtransNotification` (payment webhook)
- `models/v2/order.go` — request/DB structs

---

## 1. Endpoint

```
POST /v2/order.
```

The trailing `.` is part of the path (`routes/order.go:27`). Calling `/v2/order` without it will 404.

### Headers

| Header | Value | Required |
|---|---|---|
| `Authorization` | `Bearer <JWT>` | yes |
| `Content-Type` | `application/json` or `application/x-www-form-urlencoded` | yes |

The JWT is validated by `middlewares/v2.Authentication`; `user_id`, `email`, and `username` are read from its claims and used for the order owner and the Midtrans customer details. A missing/invalid token returns `401`.

### Query params

| Param | Description |
|---|---|
| `referer` | Optional. Stored in `order.http_referrer`. Read raw from the query string, so a full URL with its own `?`/`&` is preserved as-is. |

---

## 2. Request body

Binds to `model.Order6`:

```json
{
  "package_id": 12,
  "bundle_id": 3,
  "discount_id": 5
}
```

| Field | Type | Notes |
|---|---|---|
| `package_id` | int, nullable | Package to buy |
| `bundle_id` | int, nullable | Bundle (kelas) to buy |
| `discount_id` | int, nullable | Only applied when `bundle_id` is set |

**At least one of `package_id` / `bundle_id` is required.** If both are `null`:

```
400 Bad Request
{"message":{"warning":"harus berisi salah satu antara package_id atau bundle_id"}}
```

---

## 3. What each combination means

| Body | Meaning | Price taken from | `is_add_on` |
|---|---|---|---|
| `bundle_id` only | Buy the bundle | `bundle.price` | 0 |
| `package_id` only | Buy the package | `package.price` | 0 |
| **both** | Buy the *video pembahasan* add-on for that package inside that bundle | `bundle_package.video_price` | 1 |

Sending both IDs is **not** "buy two things" — it is the add-on purchase path. If no `bundle_package` row matches, the response is `404` (`"Bundle Package not found"`). A `NULL` `video_price` is treated as price `0`.

### Discount

Applied only when `bundle_id != null` and `discount_id > 0`. The discount row must be `is_active = 1` and either have `bundle_ids IS NULL` (applies to all bundles) or list the bundle id in its comma-separated `bundle_ids`. Otherwise the request fails with a `discount` error.

Calculation:

| Discount row | Result |
|---|---|
| `amount` only | `price - amount` |
| `percentage` only | `price - (price × percentage / 100)` |
| both | percentage off, but capped at `amount` |

---

## 4. Behaviour before an order is created

Two guards run first — both can short-circuit the request.

**a. Reuse of a still-valid order.** The handler looks up the user's newest order with the same `bundle_id` + `package_id`. If that order is not cancelled, has a `midtrans_payment_url`, was created **less than 82800s (23h)** ago, and its `amount` equals the freshly computed price, the existing payment URL is returned instead of creating a new one. Repeated POSTs within the same day therefore return the same Snap URL — this is expected, not a bug.

**b. Duplicate purchase (non add-on only).** If the user already has an active, paid, non-add-on order for the same bundle/package (for example the admin granted access via "Add User"), the request is rejected:

```
409 Conflict
{
  "message": {"warning": "Anda sudah memiliki kelas ini"},
  "data": "Anda sudah memiliki kelas ini"
}
```

The message is duplicated into `data` (as a plain string) because the frontend reads the toast text from `response.data.data`.

---

## 5. Creation flow

### Paid order (`price > 0`)

1. Insert into `order` with `uuid = "<midtrans_prefix>-<uuid-v4>"`.
   `midtrans_prefix` comes from the `midtrans_prefix` cache key, populated per-domain from the `client` table by `utils.SaveDataClientByDomain` (`utils/client.go:16`). Fallback when the cache is empty: `"ukom"`. Example UUID: `ukom-3f2a1c9e-....`
2. Call `GetOrderURL()` → Midtrans **Snap** `GetToken`, using `MIDTRANS_SERVER_KEY`, `MIDTRANS_CLIENT_KEY`, and `MIDTRANS_ENV` (anything other than the literal `midtrans.Production` means Sandbox). The order `uuid` is sent as the Midtrans `order_id`; the JWT's `email`/`username` become the customer detail.
3. Update the row with `midtrans_payment_url`, `midtrans_payment_token`, `amount`, `discount_id`, `http_referrer`, `is_add_on`.

The order is **not** paid at this point — `paid_at` stays null until the Midtrans webhook arrives.

### Free order (`price == 0`)

1. `FirstOrCreate` on `(user_id, bundle_id, package_id)` — no Midtrans call.
2. Sets `amount = 0`, `checked_out_at = now`, `http_referrer`, `is_add_on`.
3. If the bundle has **no** `bundle_field` rows, it also sets `paid_at = now` and `is_active = 1` — access is granted immediately.
   If the bundle **does** have `bundle_field` rows, `paid_at` is left null: the user must fill in those registration fields before access is granted.

---

## 6. Response

`200 OK`

```json
{
  "message": {"success": "Berhasil menambah data order"},
  "data": {
    "order": {
      "midtrans_payment_url": "https://app.sandbox.midtrans.com/snap/v3/redirection/xxxx"
    }
  },
  "error": null
}
```

For a **free order** `midtrans_payment_url` is an empty string — there is nothing to pay; redirect the user straight into the content (or to the bundle-field form).

### Error shape

All errors use the same envelope (`helpers.NewAPIResponse`):

```json
{
  "message": {"warning": "..."},
  "data": null,
  "error": {"order": "record not found"}
}
```

`message` has exactly one of `success` / `warning` / `danger`, which the frontend maps to toast severity.

| Status | When |
|---|---|
| 400 | Neither `package_id` nor `bundle_id` sent; bind/validation failure |
| 401 | Missing or invalid `Authorization` bearer token |
| 404 | Package / bundle / bundle_package / discount not found |
| 409 | User already owns this bundle or package |
| 500 | Midtrans Snap token request failed, or DB error |

---

## 7. Example

```bash
# Buy a bundle with a discount
curl -X POST 'https://<host>/v2/order.?referer=https://app.example.com/kelas/3' \
  -H 'Authorization: Bearer eyJhbGciOi...' \
  -H 'Content-Type: application/json' \
  -d '{"bundle_id": 3, "discount_id": 5}'

# Buy the video add-on for package 12 inside bundle 3
curl -X POST 'https://<host>/v2/order.' \
  -H 'Authorization: Bearer eyJhbGciOi...' \
  -H 'Content-Type: application/json' \
  -d '{"bundle_id": 3, "package_id": 12}'
```

Then redirect the browser to the returned `midtrans_payment_url`.

---

## 8. After payment — the webhook

Midtrans calls:

```
POST /v2/midtrans/notification
```

This endpoint is **public** (no auth middleware); it authenticates the caller by verifying the signature instead:

```
signature = SHA512( order_uuid + status_code + amount + ".00" + MIDTRANS_SERVER_KEY )
```

A mismatch returns `500 "signature key invalid"` and nothing is updated. On every valid call the order's `checked_out_at` is stamped and a row is written to `midtrans_notification` (including the raw payload).

Order state is then driven by `transaction_status`:

| `transaction_status` | Effect on `order` | Side effects |
|---|---|---|
| `capture` + `payment_type=credit_card` + `fraud_status=accept` | `paid_at = now`, `is_active = 1` | in-app notification + FCM push |
| `settlement` | `paid_at = now`, `is_active = 1` | in-app notification + FCM push |
| `deny`, `expire` | `expired_at = now` | — |
| `cancel` | `cancelled_at = now` | — |

So: **an order counts as purchased when `paid_at` is set.** Never treat a successful `POST /v2/order.` as a completed purchase.

---

## 9. Related endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v2/order.` | All checked-out orders of the current user, with `Package`, `Bundle`, resolved `exam_id`, and latest `transaction_id` |
| `GET` | `/v2/order/:uuid` | One order (non add-on only) + `exam_id` + `count_question` |
| `PUT` | `/v2/order/refresh-status/:id` | `:id` is the **bundle id** — clears `cancelled_at` on the user's non-add-on orders for that bundle |
| `GET` | `/v2/midtrans/check-status-order?transaction_id=...` | Proxies Midtrans' `/v2/{id}/status` API |
| `PUT` | `/v2/midtrans/cancel-order/:id` | `:id` is the **transaction id** — cancels at Midtrans, then stamps `cancelled_at` |

---

## 10. Required environment

```
MIDTRANS_ENV=            # "midtrans.Production" for live; anything else = Sandbox
MIDTRANS_SERVER_KEY=
MIDTRANS_CLIENT_KEY=
AUTH_SECRET=             # JWT signing secret
```
