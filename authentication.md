# Authentication & Authorization

How login, sessions, and access control work in this app, and how to use them when
adding features or granting access to a user.

## Overview

This app does **not** own credentials. There is no login form, no password check, and
no user lookup for authentication. All of that lives in an external auth service
(`AUTH_URL`). This app only:

1. accepts a JWT issued by the auth service,
2. verifies its signature with a shared secret (`AUTH_SECRET`),
3. keeps it in a cookie session,
4. gates every request against an in-memory RBAC cache.

Everything runs through a single middleware: `middleware.AuthMiddleware` in
`middleware/authMiddleware.go:29`.

| Concern | Owner |
| --- | --- |
| User credentials, login page, password reset | Auth service (`AUTH_URL`) |
| Token issuing and refreshing | Auth service |
| Token verification | This app (`auth/service.go:39`) |
| Session | This app (cookie session, `routes/index.go:24`) |
| Access rules (which user may hit which route) | Auth service DB, read here via the `_access` view |
| Access enforcement | This app (`utils/rbac.go`, `middleware/authMiddleware.go`) |

## Configuration

Set these in `.env` (required — it is embedded at build time, see `README.md`).

| Variable | Description |
| --- | --- |
| `AUTH_URL` | Base URL of the auth service. Used for the login redirect, token refresh, and the `/v2/env` password-policy lookup. |
| `AUTH_SECRET` | HMAC secret used to verify the JWT. **Must match the auth service.** |
| `AUTH_CLIENT_ID` | This client's numeric ID. Scopes which access rows apply, and is also the tenant filter for most feature queries. |
| `AUTH_RBAC` | `true` = rule-based access from the `_access` view. `false` = flat allowlist via `AUTH_ALLOWED_USERS`. |
| `AUTH_ALLOWED_USERS` | Comma-separated user IDs allowed when `AUTH_RBAC=false`. Ignored when RBAC is on. |
| `AUTH_ALLOWED_ROUTES` | Comma-separated paths that skip the per-route RBAC check when `AUTH_RBAC=true`. The user must still be authenticated and still needs at least one access row. |

A minimal working set for local development against a staging auth service:

```dotenv
AUTH_URL=https://auth.example.com
AUTH_SECRET=<same secret as the auth service>
AUTH_RBAC=false
AUTH_CLIENT_ID=3
AUTH_ALLOWED_USERS=1,42
AUTH_ALLOWED_ROUTES=/dashboard,/logout
```

With `AUTH_RBAC=false` you skip needing access rows in the database entirely — useful
when developing a new feature before its routes have been registered with the auth
service.

## The login flow

There is no `/login` route in this app. The flow is:

1. A browser hits any protected route without a session.
2. The middleware redirects to
   `AUTH_URL/v2/auth/login?client_base_url=<scheme>://<host><original URI>`
   (`middleware/authMiddleware.go:77`).
3. The auth service authenticates the user and redirects back to this app with
   `?access_token=<jwt>&pass=<path to land on>`.
4. The middleware picks that up (`middleware/authMiddleware.go:36`), verifies the token,
   checks the user has at least one access entry, stores `token` / `email` / `name` in
   the session, and redirects to `pass`.
5. Every later request is authenticated from the session cookie.

The JWT claims this app relies on are `user_id`, `email`, `name`, and `expired_at`.

### Token refresh

The middleware compares `expired_at` against the current time on every request
(`middleware/authMiddleware.go:179`). When expired, it calls
`AUTH_URL/oauth/refresh-token?token=<current token>` inline and swaps the returned
`access_token` into the session. Any failure — network error, non-200, missing
`access_token` — clears the session and redirects to login.

This is a blocking HTTP call on the request path, so a slow or unreachable auth service
stalls requests for users whose token has just expired.

### Logout

`GET /logout` (`routes/uncategorized.go:24`) clears the local session and redirects to
the auth service login page. It does **not** invalidate the token upstream, so a copy of
the token remains valid until it expires.

## Protecting a route

Attach `middleware.AuthMiddleware(db)` to every route. Follow the existing per-feature
pattern (see `routes/version.go` for a compact example):

```go
func RouteThing(router *gin.Engine, db *gorm.DB) *gin.RouterGroup {
	thing := router.Group("thing")
	{
		thing.GET(".", middleware.AuthMiddleware(db), func(c *gin.Context) {
			handler.GetThing(c, db)
		})
		thing.GET("/create", middleware.AuthMiddleware(db, "modal"), func(c *gin.Context) {
			handler.GetThingCreate(c)
		})
		thing.POST("/create", middleware.AuthMiddleware(db), func(c *gin.Context) {
			handler.PostThingCreate(c, db)
		})
		thing.GET("/view/:id", middleware.AuthMiddleware(db), func(c *gin.Context) {
			handler.GetThingView(c, db)
		})
		thing.GET("/datatable", middleware.AuthMiddleware(db), func(c *gin.Context) {
			handler.GetThingDatatable(c, db)
		})
	}
	return thing
}
```

Then register `RouteThing(router, db)` in `routes/index.go` `GetGinRoute()` — routes are
not auto-discovered.

### The `"modal"` variant

`AuthMiddleware(db, "modal")` changes only what a **denied** request renders:

- without the argument → full page `error_forbidden.html`
- with any extra argument → `error_forbidden_modal.html`, written directly to the
  response

Use it on endpoints whose HTML is loaded into a Bootstrap modal (typically
`GET /create` and `GET /update/:id`), so a forbidden response renders inside the modal
instead of injecting a whole page layout. The string value is not inspected — the code
only checks `len(state) != 0` — but the codebase consistently passes `"modal"`.

The POST counterparts use the plain form, since they are not rendered into a modal.

### Route naming matters

RBAC matching normalizes the request path by stripping trailing digits and slashes
(`utils/rbac.go:78`), so `/thing/view/42` is matched as `/thing/view/`. Keep IDs as the
last path segment. A route like `/thing/42/view` will not normalize the way the access
rules expect.

## Reading the current user

The middleware puts two things into the Gin context.

**`userData`** — the raw JWT claims, for handlers that need the user ID:

```go
userData := c.MustGet("userData").(jwt.MapClaims)
userID := int32(userData["user_id"].(float64))
```

Note the claim is a `float64` (JSON numbers), so the double conversion is required. See
`web/handlers/version.go:88` for a live example.

**`global`** — a `model.Global` with the display identity, passed into every template:

```go
global := c.MustGet("global").(model.Global)
c.HTML(http.StatusOK, "thing_index.html", gin.H{"global": global})
```

In templates: `{{.global.Email}}`, `{{.global.Name}}`, and `{{.global.TimeCache}}` —
the last is a startup timestamp used as a cache-buster on asset URLs
(`web/templates/layouts/base.html`).

Other context keys set upstream in `routes/index.go:37`: `sess` (storage client), `acc`
(the access cache), `form`, and `time_cache`.

## How authorization is decided

### `AUTH_RBAC=false` — flat allowlist

The user ID must appear in `AUTH_ALLOWED_USERS`. Otherwise the session is cleared and
the user is bounced to login. No per-route checking at all — anyone on the list can
reach every route.

### `AUTH_RBAC=true` — rule-based

Rules come from the `_access` view and are loaded into a `go-cache` **once at startup**
by `utils.SaveDataAccess` (`routes/index.go:23`). Only rows whose `client_id` matches
`AUTH_CLIENT_ID`, or is NULL/empty, are loaded.

Two checks run per request:

1. **Has any access at all** (`utils.UserHasAnyAccess`) — a user with zero rows for this
   client is not an admin of this app and is sent back to login.
2. **Has access to this specific route** (`utils.FindDataAccess`), skipped for paths
   listed in `AUTH_ALLOWED_ROUTES`.

`FindDataAccess` (`utils/rbac.go:66`) resolves in this order:

| Order | Cache key | Meaning |
| --- | --- | --- |
| 1 | `user_id*:{id}` | Superuser: has a row with `route_name='*'` and `route_method='*'`. Everything is allowed. |
| 2 | `user_id:{id}\|route_name:{route}\|route_method:{method}` | Exact grant for the normalized route and HTTP method. |
| 3 | `user_id:{id}` | List of wildcard rules. Matches when `route_name='*'` and the method matches, or when the request path starts with the rule's prefix (rule `/thing/*` → prefix `/thing`) and the method matches or is `*`. |

First match wins. No match → `error_forbidden.html` (or the modal variant).

There is also a rule-function key, `user_id:{id}|function:{rule_function}`, populated
from rows with a non-empty `rule_id`. It is loaded into the cache for feature-level
checks and is not consulted by the route check.

### Special case: question assignment

`utils/rbac.go:112` grants access without an explicit rule for a specific set of
question and package routes, when the user has questions assigned to them
(`question.assigned_to = user_id`):

- List/index routes (`/question/`, `/question/datatable/`, `/package/`,
  `/package/datatable/`, `/package/view/`, `/package/question/datatable/`,
  `/package/question/view/`) are allowed if the user has **any** assigned question.
- Detail routes (`/question/update/`, `/question/view/`, `/question/review/`) are
  allowed only for the specific question ID assigned to that user.

The "has any assignment" result is memoized as `assigned_to_user_id:{id}` and never
expires, so revoking every assignment from a user does not take effect until restart.

## Granting access to a user

`_access` is a **database view** (`schema.sql:46`), so you cannot grant access by
inserting into it. The underlying role/route/rule rows are managed by the auth service —
add or change them there, for the client ID matching this app's `AUTH_CLIENT_ID`.

The columns this app reads are `user_id`, `client_id`, `client_base_url`, `route_name`,
`route_method`, `rule_id`, and `rule_function`.

Typical shapes:

| Goal | `route_name` | `route_method` |
| --- | --- | --- |
| Full admin | `*` | `*` |
| Everything under a feature | `/thing/*` | `*` |
| Read-only on a feature | `/thing/*` | `GET` |
| One specific page | `/thing/view/` | `GET` |

To verify what the app actually sees for a user:

```sql
SELECT user_id, client_id, route_name, route_method, rule_function
FROM _access
WHERE user_id = 42
  AND (client_id = <AUTH_CLIENT_ID> OR client_id IS NULL OR client_id = '');
```

### Applying rule changes

The cache is built once at startup, so **new or changed rules do not take effect
immediately**. Either:

- restart the app, or
- hit `GET /reload-cache` (`routes/uncategorized.go:41`), which rebuilds the cache from
  the database.

Be aware that `/reload-cache` has no auth middleware — it is reachable anonymously.

## The weak-password gate

Independent of RBAC. When the session holds `password_validity = "weak"`, the middleware
(`middleware/authMiddleware.go:93`) redirects every request except `/logout` and
`/setting/change-password` to the change-password page.

The flag is set in `web/handlers/password.go:50`: when `/setting/change-password` is
opened with a `?password_validity=` query parameter, the handler fetches
`AUTH_URL/v2/env` and, if that returns `auth_password_update_force=true`, marks the
session weak. If the auth service is unreachable the fetch fails open — the flag is not
set and the user is not locked into the change-password page.

The password change itself is done by the page's JavaScript, not by a Go handler:
`web/templates/password/password_edit.html` sends `PUT <auth>/v2/user/set-password` with
the session token as a bearer token, then calls `PUT /setting/change-password` on this
app purely to clear the flag.

Gotcha when testing: that template does **not** use `AUTH_URL`. It picks the auth host by
sniffing `window.location.hostname` — `localhost` → `http://localhost:8001`, a hostname
containing `dev` → `https://dev-auth.appskep.id`, anything else →
`https://auth.appskep.id`. Running against a staging auth service from a hostname that
does not match those patterns will hit the wrong host.

## Troubleshooting

**Redirect loop between the app and the login page.** Usually `AUTH_SECRET` does not
match the auth service, so every token fails verification. Also check `AUTH_CLIENT_ID`
against the access rows: with `AUTH_RBAC=true`, a user with zero matching rows is
redirected to login on every attempt, which looks identical to a bad token.

**Forbidden page on a route that should be allowed.** The rule exists but the cache is
stale — restart or hit `/reload-cache`. If it persists, check the normalized path: the
request `/thing/view/42` is looked up as `/thing/view/`, and the method must match
exactly or be `*`.

**Works for one user, forbidden for another.** Compare their `_access` rows with the
query above. A superuser (`*`/`*`) row short-circuits everything, which can mask missing
per-route rules during testing.

**`c.MustGet("global")` panics in a handler.** The route is missing
`middleware.AuthMiddleware(db)` — `global` is only set there.

**New feature returns 404, not a redirect.** The `RouteX` call was not added to
`routes/index.go` `GetGinRoute()`.

**Everyone is stuck on the change-password page.** The auth service is returning
`auth_password_update_force=true`. Sessions clear the flag only via
`PUT /setting/change-password`.

## Security notes

Known weak points in the current implementation, worth knowing before relying on this
for anything sensitive:

- **The session cookie key is hardcoded** to the literal `[]byte("secret")` in
  `routes/index.go:24`, and it is committed to the repository. Anyone who knows it can
  forge a session cookie. A forged cookie still has to carry a token that verifies
  against `AUTH_SECRET`, so this is not a direct authentication bypass — but `email` and
  `name` are read from the session in preference to the JWT claims
  (`middleware/authMiddleware.go:105`), so the displayed identity is attacker
  controllable. Move this to an environment variable with a random value.
- **The token travels as a URL query parameter** on the redirect back from the auth
  service, so it lands in browser history, `Referer` headers, and this app's own request
  log.
- **The change-password page embeds the raw token in the HTML** via `{{.token}}`
  (`web/templates/password/password_edit.html:263`), where any script on the page can
  read it.
- **`GET /reload-cache` is unauthenticated** — any anonymous caller can trigger a full
  RBAC reload from the database.
- **Forbidden responses call `c.Abort()` without returning**
  (`middleware/authMiddleware.go:156`), so the remainder of the middleware body — token
  refresh and request logging — still runs for a denied request.
- **Logout is local only.** The token remains valid at the auth service until it
  expires.
