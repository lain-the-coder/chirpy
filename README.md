# Chirpy

A JSON REST API for a Twitter-like microblogging service, written in Go against the standard library — no web framework, no ORM. Users register, authenticate, and post short messages ("chirps"); a payment provider upgrades them to a paid tier over a webhook.

On the surface it's a Twitter clone. Underneath it's a study in the concerns that define a real backend service: a stateless/stateful two-token auth system, password hashing chosen against an actual threat model, three distinct authorization patterns, database-enforced integrity, and a routing layer built from `net/http` primitives rather than a framework. This README documents the design decisions and their reasoning, not just the endpoint list.

---

## Contents

- [Feature Overview](#feature-overview)
- [API Reference](#api-reference)
- [Architecture](#architecture)
- [Authentication & Authorization](#authentication--authorization)
- [Design Decisions](#design-decisions)
- [Data Model](#data-model)
- [Project Layout](#project-layout)
- [Running It](#running-it)
- [Testing](#testing)
- [What I'd Build Next](#what-id-build-next)
- [Concepts Demonstrated](#concepts-demonstrated)

---

## Feature Overview

- **User accounts** — registration, login, and self-service credential updates, with Argon2id password hashing.
- **Two-token authentication** — short-lived stateless JWT access tokens (1 hour) paired with long-lived, database-backed, revocable refresh tokens (60 days).
- **Chirps** — create, read, list, and delete short posts, with length limits and profanity filtering.
- **Resource-level authorization** — you may only delete your own chirps, only update your own account, and only ever post as yourself.
- **Filtering and sorting** — `GET /api/chirps` accepts optional `author_id` and `sort` query parameters.
- **Webhook integration** — a payment provider ("Polka") upgrades users to a paid tier over an API-key-authenticated webhook.
- **Admin namespace** — request metrics and an environment-gated destructive reset endpoint.

---

## API Reference

### Public

| Method | Path                    | Description                                                                                              |
| ------ | ----------------------- | -------------------------------------------------------------------------------------------------------- |
| `GET`  | `/api/healthz`          | Readiness check. Returns `200 OK` / `text/plain`.                                                        |
| `POST` | `/api/users`            | Register. Body: `{email, password}` → `201` with the user resource.                                      |
| `POST` | `/api/login`            | Authenticate. Body: `{email, password}` → `200` with the user resource plus `token` and `refresh_token`. |
| `GET`  | `/api/chirps`           | List chirps. Optional `?author_id=<uuid>` and `?sort=asc\|desc` (default `asc`).                         |
| `GET`  | `/api/chirps/{chirpID}` | Fetch one chirp. `404` if it doesn't exist.                                                              |

### Authenticated (`Authorization: Bearer <access-token>`)

| Method   | Path                    | Description                                                                           |
| -------- | ----------------------- | ------------------------------------------------------------------------------------- |
| `POST`   | `/api/chirps`           | Post a chirp. Body: `{body}` — the author is taken from the token, never the payload. |
| `PUT`    | `/api/users`            | Update your own email and password.                                                   |
| `DELETE` | `/api/chirps/{chirpID}` | Delete a chirp you authored. `403` if you didn't.                                     |

### Session management (`Authorization: Bearer <refresh-token>`)

| Method | Path           | Description                                                |
| ------ | -------------- | ---------------------------------------------------------- |
| `POST` | `/api/refresh` | Exchange a valid refresh token for a fresh access token.   |
| `POST` | `/api/revoke`  | Revoke a refresh token, ending the session. Returns `204`. |

### Machine-to-machine (`Authorization: ApiKey <polka-key>`)

| Method | Path                  | Description                                                                             |
| ------ | --------------------- | --------------------------------------------------------------------------------------- |
| `POST` | `/api/polka/webhooks` | Payment-provider webhook. Upgrades a user on `user.upgraded`; ignores all other events. |

### Admin & static

| Method | Path             | Description                                                      |
| ------ | ---------------- | ---------------------------------------------------------------- |
| `GET`  | `/admin/metrics` | HTML page showing the fileserver hit count.                      |
| `POST` | `/admin/reset`   | Deletes all users. **Refuses with `403` unless `PLATFORM=dev`.** |
| `GET`  | `/app/*`         | Static fileserver, wrapped in the metrics-counting middleware.   |
| `GET`  | `/`              | Redirects to `/app/`.                                            |

---

## Architecture

The server is `net/http` and nothing else — no Gin, no Chi, no Echo. Routing is a `*http.ServeMux` using Go 1.22's method-aware patterns (`"POST /api/chirps"`) and path wildcards (`"/api/chirps/{chirpID}"`, read back via `r.PathValue`). The exact-match wildcard `"GET /{$}"` pins the root redirect to the literal root path so it doesn't shadow every other route as a subtree pattern.

```
                    ┌──────────────────────────────────────────┐
   HTTP request ───▶│  http.Server                              │
                    │   → mux.ServeHTTP  (method + path match)  │
                    └────────────────────┬─────────────────────┘
                                         │
                    ┌────────────────────┴─────────────────────┐
                    │                                           │
                    ▼                                           ▼
        ┌───────────────────────┐              ┌───────────────────────────┐
        │  middlewareMetricsInc │              │  handlers  (methods on    │
        │  (wraps /app/ only)   │              │  *apiConfig)              │
        └───────────┬───────────┘              └─────────┬─────────────────┘
                    │                                     │
                    ▼                                     │
        ┌───────────────────────┐                         │
        │  http.FileServer      │                         │
        └───────────────────────┘                         │
                                                          │
                    ┌─────────────────────────────────────┼──────────────────┐
                    │                                     │                  │
                    ▼                                     ▼                  ▼
        ┌───────────────────────┐        ┌────────────────────────┐  ┌──────────────┐
        │  internal/auth        │        │  *database.Queries     │  │  apiConfig   │
        │  Argon2id · JWT ·     │        │  (SQLC-generated)      │  │  shared      │
        │  refresh tokens ·     │        │        │               │  │  state       │
        │  header parsing       │        │        ▼               │  └──────────────┘
        └───────────────────────┘        │   PostgreSQL           │
                                         └────────────────────────┘
```

**Shared state lives in one `apiConfig` struct**, constructed once in `main()` and reached by every handler through a pointer receiver. It carries the request counter, the database handle, the environment flag, the JWT signing secret, and the Polka API key.

This is a deliberate contrast with the framework model many backends use. Go's `net/http` does **not** instantiate a fresh handler object per request the way ASP.NET scopes a controller — it spawns a goroutine per request and hands every one of them the _same_ handler instance. Sharing is the default; isolation is opt-in. That's why the hit counter is an `atomic.Int32` rather than a plain `int`: it's genuinely written concurrently by many in-flight requests, and a naive `++` would be a data race.

**Middleware is `func(http.Handler) http.Handler`** — a function that takes a handler and returns a new one wrapping it, using a closure to capture the wrapped handler. Because `*http.ServeMux` is itself an `http.Handler`, the exact same middleware can wrap a single endpoint or the entire router without modification. Here the metrics middleware deliberately wraps _only_ the fileserver: wrapping the whole mux would make `/admin/metrics` increment the very counter it reports.

---

## Authentication & Authorization

These are two different questions, and the codebase treats them as such: **authentication** establishes _who you are_; **authorization** decides _what you may touch_.

### The two-token model

A stateless JWT is fast to verify — no database round-trip, just a signature recomputation — but for exactly that reason it is **irrevocable**. Once issued, nothing can stop it until it expires. A long-lived access token is therefore a long-lived skeleton key for whoever steals it.

The resolution is to split the job:

|                 | Access token             | Refresh token                                |
| --------------- | ------------------------ | -------------------------------------------- |
| **Format**      | JWT (HS256, signed)      | 256-bit random hex string                    |
| **Storage**     | None — stateless         | A row in `refresh_tokens`                    |
| **Lifetime**    | 1 hour                   | 60 days                                      |
| **Used for**    | Every authenticated call | Only to mint a new access token              |
| **Revocable**   | No                       | **Yes** — `revoked_at` timestamp             |
| **Verified by** | Signature + `exp` claim  | Database lookup + expiry + revocation checks |

The refresh token is deliberately **not** a JWT. The entire value of a JWT's signature is verification _without_ a database lookup — but a revocable token must be looked up, and once you're doing that, the row itself is the proof. Signing it would be ceremony. It is simply an unguessable random string from `crypto/rand`, which matters more here than for a JWT: a refresh token has no signature protecting it, so unguessability is its _only_ defense.

Revocation isn't instantaneous — an already-issued access token stays valid until its `exp`. That residual window is precisely why the access token lifetime is one hour and not one week.

### Three authorization patterns

The codebase uses three distinct shapes, and the right one depends on how far the _actor_ is from the _resource_:

1. **Identity substitution** — `POST /api/chirps`. The request body has no `user_id` field at all; the author comes from the token's `sub` claim. There is nothing to verify because there is nothing to lie about. A client-supplied identity is not evidence of anything, and cross-checking it against the token would add a field and a failure mode for zero security.

2. **Query-scoped** — `PUT /api/users`. The `WHERE id = $1` clause _is_ the authorization boundary: `$1` is the token's subject, so the statement is structurally incapable of touching another user's row.

3. **Fetch-then-compare** — `DELETE /api/chirps/{chirpID}`. Chirps are publicly readable but owner-only deletable, so the actor and the resource are different entities. The row is fetched, its `user_id` compared against the token's subject, and a mismatch returns `403`. Folding this into `WHERE id = $1 AND user_id = $2` would be _safe_ but _opaque_ — it couldn't distinguish "doesn't exist" (`404`) from "not yours" (`403`), which the spec requires.

### Status codes carry meaning

- **`401`** — "I don't know who you are." Missing, malformed, expired, or forged credentials. A different token might fix it.
- **`403`** — "I know exactly who you are, and no." Identity is established; permission is not. No token fixes it.

All authentication failures collapse into an indistinguishable `401` with an identical body. Login does the same with "no such user" and "wrong password" — a differentiated response would let an attacker enumerate registered emails from an unauthenticated position. Once a caller _has_ proven their identity, precise errors become a feature rather than a leak: `403` on someone else's chirp reveals nothing they couldn't read from the public timeline anyway.

### API keys for machines

The Polka webhook can't use any of the above. Polka is not a user — no account, no password, no JWT. It's a _system_, and it authenticates with a shared API key (`Authorization: ApiKey <key>`) verified by string equality against a value in `.env`.

This endpoint is worth calling out because it was, briefly, wide open. The auth reflex is "authenticate the _user_" — and there is no user here, so nothing fired. Anyone who knew the URL could have granted themselves a paid membership, and user IDs aren't even secret (they're returned on every chirp). The correct question for any state-mutating endpoint is not _"did I authenticate the user?"_ but **_"do I know who is calling this, and do I trust them?"_**

---

## Design Decisions

**Argon2id for passwords, not SHA-256.** These are not interchangeable, and the difference is the threat model. SHA-256 and MD5 are _fast by design_ — that's what makes them good for checksums and catastrophic for passwords, because an attacker who steals the database can run billions of guesses per second against every stolen hash. Argon2id is a key-derivation function: deliberately slow and memory-hard. That cost is imperceptible for one legitimate login and devastating across a brute-force campaign. The asymmetry _is_ the security property.

**`crypto/rand`, never `math/rand`.** Identical signatures, catastrophically different guarantees. `math/rand` is a deterministic PRNG whose output is predictable given enough of it — fine for shuffling, fatal for a refresh token whose only protection is being unguessable.

**Response structs are decoupled from database structs.** Every handler maps the SQLC-generated row into a dedicated response type, copying only the fields it means to expose. `hashed_password` is a real field on the `User` struct — and it is _structurally impossible_ for it to leak, because no response struct has a slot for it. That's a compile-time guarantee, not a runtime discipline you have to remember on every endpoint.

**Do everything that can fail _before_ committing to a status code.** `w.WriteHeader` is a one-way door: the first call wins, every subsequent one is silently discarded. `respondWithJSON` therefore marshals _first_ and writes the status _after_ — an earlier version wrote the status first, so a marshalling failure would send `500` to the logs while the client received the original status code and an empty body.

**Webhook status codes are a control signal, not a description.** Polka retries on any non-2XX. An unrecognized event therefore returns **`204`**, not an error — returning `400` for an event you structurally never handle would create an infinite retry loop over a message that can never succeed. Reserve error codes for failures a retry could plausibly fix.

**SQLC and Goose, not an ORM.** Migrations are versioned, reversible `.sql` files with explicit up/down halves. Queries are hand-written SQL, and SQLC generates type-safe Go bindings from them. Nothing is hidden: the SQL that runs is the SQL in the repo.

**`:one` vs `:exec` determines what failures you can even see.** An `UPDATE`/`DELETE` matching zero rows is not an error in Postgres — it's a successful statement that affected nothing, and an `:exec` query reports `nil`. Only a query with a `RETURNING` clause (`:one`) surfaces `sql.ErrNoRows`. This is why `UpgradeUser` is `:one`: without it, the "user not found → `404`" requirement would be unsatisfiable without a second query. The same error branch that is load-bearing there was dead, unreachable code in `DeleteChirp`, and was removed.

**`NULL` means absence, and only absence.** `revoked_at` is nullable because "not revoked" _is_ the absence of a value; the presence of a timestamp _is_ the revocation. `is_chirpy_red` is `NOT NULL DEFAULT FALSE` because `false` is a real, true fact about a user, not a placeholder. Revocation is a soft delete rather than a row deletion, preserving the audit trail of when a session ended.

**Constraints belong in the database.** `email UNIQUE`, `user_id ... REFERENCES users(id) ON DELETE CASCADE`, `NOT NULL` — these are enforced atomically by Postgres, closing race windows application code cannot. `ON DELETE CASCADE` is why `/admin/reset` correctly cleans up chirps and refresh tokens with no extra code.

**Destructive operations are idempotent and information-free.** `/api/revoke` returns `204` whether the token existed, was already revoked, or was garbage — it's neither a retry-punishing error nor an enumeration oracle. `/admin/reset` is gated on an explicit `PLATFORM=dev` check rather than obscurity: in production the flag is simply never set, so the endpoint is structurally unreachable rather than merely hard to find.

**Secrets in `.env`, gitignored, always.** The JWT signing secret is generated with `openssl rand -base64 64`. Whoever holds it can forge a valid token for any user — it belongs in the same category as the database password. Note the difference in provenance: the JWT secret is _ours_ and never leaves the server, while the Polka key is _theirs_, arrives over the network on every request, and therefore makes HTTPS load-bearing rather than optional.

---

## Data Model

```
users                             chirps
├── id            UUID  PK        ├── id          UUID  PK
├── created_at    TIMESTAMP       ├── created_at  TIMESTAMP
├── updated_at    TIMESTAMP       ├── updated_at  TIMESTAMP
├── email         TEXT  UNIQUE    ├── body        TEXT
├── hashed_password  TEXT         └── user_id     UUID  FK ──┐
└── is_chirpy_red   BOOLEAN                                   │
        ▲                                                     │
        │  ON DELETE CASCADE                                  │
        ├─────────────────────────────────────────────────────┘
        │
        │                          refresh_tokens
        │                          ├── token       TEXT  PK
        │                          ├── created_at  TIMESTAMP
        │                          ├── updated_at  TIMESTAMP
        └──────────────────────────┤ user_id     UUID  FK
                                   ├── expires_at  TIMESTAMP
                                   └── revoked_at  TIMESTAMP  NULL = not revoked
```

`refresh_tokens` uses the token string itself as its primary key — it's a 256-bit random value, so uniqueness is guaranteed by construction, and every query against the table looks it up by exactly that. A surrogate key would be a column nothing ever selects on.

---

## Project Layout

```
chirpy/
├── main.go                       # apiConfig, handlers, middleware, routing, response helpers
├── sqlc.yaml                     # SQLC codegen config
├── .env                          # DB_URL, PLATFORM, SECRET, POLKA_KEY  (gitignored)
├── index.html                    # served at /app/
├── assets/
├── sql/
│   ├── schema/                   # Goose migrations (up/down)
│   │   ├── 001_users.sql
│   │   ├── 002_chirps.sql
│   │   ├── 003_hashed_password.sql
│   │   ├── 004_refresh_tokens.sql
│   │   └── 005_is_chirpy_red.sql
│   └── queries/                  # hand-written SQL → SQLC input
│       ├── users.sql
│       ├── chirps.sql
│       └── refresh_tokens.sql
└── internal/
    ├── auth/
    │   ├── auth.go               # hashing, JWT create/validate, refresh tokens, header parsing
    │   └── auth_test.go
    └── database/                 # SQLC-generated — do not edit by hand
```

`internal/auth` is a deliberate boundary. It is the only package that imports `argon2id` or `golang-jwt` — handlers depend on _its_ functions, never on the underlying libraries. Swapping the hashing algorithm or the JWT library tomorrow touches exactly one file.

Its functions also take the narrowest type they need: `GetBearerToken(headers http.Header)` rather than `(*http.Request)`. The auth package has no business knowing what an HTTP _request_ is, and the narrower signature makes it trivially testable — construct an `http.Header{}` by hand, no fake request required.

---

## Running It

Requires Go 1.22+ (the routing depends on method-aware `ServeMux` patterns), PostgreSQL, [Goose](https://github.com/pressly/goose), and [SQLC](https://sqlc.dev).

```bash
git clone https://github.com/lain-the-coder/chirpy.git
cd chirpy
go mod download
```

Create the database:

```bash
psql postgres          # or: sudo -u postgres psql
CREATE DATABASE chirpy;
```

Create `.env` in the project root (and confirm it's gitignored):

```dotenv
DB_URL="postgres://<user>:<password>@localhost:5432/chirpy?sslmode=disable"
PLATFORM="dev"
SECRET="<openssl rand -base64 64>"
POLKA_KEY="<key provided by the payment provider>"
```

> `PLATFORM=dev` unlocks `POST /admin/reset`, which **deletes every user**. It exists so the endpoint is structurally unreachable in any environment where that flag isn't set.

Apply the migrations, from the schema directory:

```bash
cd sql/schema
goose postgres "$DB_URL" up
cd ../..
```

Regenerate the database bindings whenever a query or migration changes, from the project root:

```bash
sqlc generate
```

Run it:

```bash
go run .
```

The server listens on `:8080`.

---

## Testing

```bash
go test ./...
```

`internal/auth` is covered by unit tests, including — deliberately — the paths that must **fail**:

- a JWT round-trips and yields back the same user ID,
- an **expired** token is rejected,
- a token signed with the **wrong secret** is rejected,
- `Authorization` header parsing handles the valid, missing, and malformed cases.

The two rejection tests assert `err != nil` — the _presence_ of an error is the passing condition. A security check that never fires is not a check.

---

## What I'd Build Next

Honest notes on where this would go with more time, rather than pretending it's finished:

- **Panic-recovery middleware.** An unrecovered panic in a handler currently produces a raw stack trace in the logs and an abruptly-closed connection instead of a clean `500`. A `defer`/`recover` wrapper around `next.ServeHTTP` is a dozen lines and belongs on the whole mux. This isn't hypothetical — an out-of-range slice index in `GET /api/chirps` demonstrated exactly this failure mode during development.
- **Handler-level tests with `httptest`.** Auth is unit-tested; the handlers are not. `httptest.ResponseRecorder` would let the routing, status codes, and JSON shapes be asserted in isolation, without a live server or database.
- **Widen `PUT /api/users`.** Its query is `RETURNING email`, so the endpoint can only return `{email}` — not the full user resource, and not `is_chirpy_red`. That premature narrowing has now blocked two separate response-shape changes; the fix is `RETURNING *`. A standing lesson: don't narrow a `RETURNING` clause to save bytes you'll pay back in optionality.
- **Pagination — and the sorting rework it forces.** `?sort=desc` currently sorts in memory with `sort.Slice`. That's fine at this scale and _semantically broken_ the moment pagination exists: you cannot sort a page you haven't fetched. Sorting has to move into the SQL `ORDER BY`, which is awkward precisely because a sort direction can't be a bind parameter.
- **Constant-time secret comparison.** The Polka API key is checked with `!=`, which short-circuits on the first differing byte and therefore leaks timing information. `crypto/subtle.ConstantTimeCompare` is the correct tool. The timing signal is unmeasurable over a network here, but it's the same family of "the naive tool leaks" as `math/rand` vs `crypto/rand`.
- **Refresh-token rotation.** Redeeming a refresh token currently leaves it valid for its full 60 days. Issuing a new one and revoking the old one on every refresh would sharply limit the damage from a stolen token.
- **Reconcile `is_chirpy_red`'s generated type.** SQLC emitted it as `sql.NullBool` despite the column being `NOT NULL` — meaning the schema and the generated code disagree. Worth chasing down; it should collapse to a plain `bool`.
- **Rate limiting** on `/api/login`, which is currently an unthrottled password-guessing oracle.

---

## Concepts Demonstrated

`net/http` routing without a framework (method patterns, path wildcards, exact-match `{$}`) · the `Handler`/`HandlerFunc` adapter and middleware as `func(Handler) Handler` · closures capturing state across a request boundary · concurrency-safe shared state with `sync/atomic` · dependency injection over package-level globals · Argon2id password hashing and the threat model behind it · JWT creation and verification (HS256, claims, `keyFunc` callbacks) · stateless vs. stateful token design · refresh tokens, revocation, and soft deletes · API-key authentication for machine callers · three distinct authorization patterns · webhook handling and retry semantics · Goose migrations and SQLC codegen · foreign keys, `ON DELETE CASCADE`, and `NULL` semantics · sentinel errors and `errors.Is` · error wrapping with `%w` · optional JSON fields via pointers and `sql.NullX` · query-parameter filtering and sorting · `context.Context` threaded through every database call · table-driven unit tests including negative-path security assertions.
