# ADR-0006: Browser Session Layer

**Date:** 2026-04-13
**Status:** Accepted
**Amends:** ADR-0001 (additive — no regression to API key primitive)

---

## Context

ADR-0001 defines two-scope API keys as the backend auth primitive. That model is correct for CLI and initramfs clients. It is unergonomic for the web UI: raw admin key in `localStorage` is JS-accessible, survives XSS, and is visible to browser extensions. A proper browser session layer is needed.

A quick-fix exists (Dinesh agent a64bee0a): admin key pasted into a modal, stored in `localStorage`, attached as `Authorization: Bearer` on every fetch. This ships immediately to unblock the founder and is the migration baseline. It is explicitly temporary.

---

## Decision

### Login endpoint

```
POST /api/v1/auth/login
Content-Type: application/json

{ "key": "<raw-admin-key>" }
```

Server validates the key against the `api_keys` table (same SHA-256 hash lookup as Bearer middleware, `scope=admin` only — node keys are rejected). On success, creates a session and returns:

```
Set-Cookie: clonr_session=<signed-token>; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=43200
```

Response body: `{ "ok": true }`. The raw key is never echoed back.

### Session token format — stateless signed token (JWT-like)

Choice: **stateless HMAC-signed token** over a server-side session table.

Justification: clonr targets single-node or small-cluster deployments with no shared session store. A server-side table adds a write on every request (sliding expiry update) and a read on every authenticated request — two DB round-trips per call on a SQLite-backed server is acceptable, but the operational surface (table migration, cleanup job) adds complexity with no proportional benefit at this scale. A stateless token eliminates the table entirely.

Token structure (base64url-encoded JSON envelope, HMAC-SHA256 signed):

```
header.payload.signature

payload: {
  "kid":    "<key_prefix>",   // 8-char prefix of the admin key used to log in
  "scope":  "admin",
  "iat":    <unix>,
  "exp":    <unix + 43200>,   // absolute expiry: 12 hours
  "slide":  <unix>            // last activity timestamp, updated on each response
}
```

**Sliding expiry:** on each authenticated response, if `now - slide > 30m`, the server re-signs and re-issues the cookie with an updated `slide` and a fresh `Max-Age=43200`. A session that goes idle for 12 hours expires absolutely; active sessions never expire mid-use.

Server secret: `CLONR_SESSION_SECRET` env var (32+ random bytes). Rotatable — rotation invalidates all sessions, users re-login.

### Middleware resolution — two auth sources, one context result

The auth middleware checks in order:

1. `Cookie: clonr_session=<token>` — validates HMAC signature, checks `exp`, extracts scope.
2. `Authorization: Bearer <token>` — SHA-256 hash lookup against `api_keys`, extracts scope.

Both paths produce an identical `AuthContext{Scope, KeyPrefix}` struct. All downstream handlers see only this struct — they are unaware of which auth source resolved it. The two paths are strictly additive; neither replaces the other.

Node-scope keys never produce a session cookie (login endpoint rejects them). The initramfs path is unaffected.

### Session TTL

- **Absolute TTL:** 12 hours from login (`exp`)
- **Sliding window:** reset on activity every 30 minutes (re-issued cookie)
- **Rationale:** HPC operators work in shifts; 12h covers a full shift without forcing re-login. 30-minute slide granularity limits re-sign overhead to at most 24 writes per session lifetime.

### Logout endpoint

```
POST /api/v1/auth/logout
```

Requires a valid session cookie. Server responds with:

```
Set-Cookie: clonr_session=; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=0
```

Because the token is stateless, "revocation" is cookie deletion on the client. For hard revocation (compromised session), the operator rotates `CLONR_SESSION_SECRET`, which invalidates all outstanding tokens globally. This is acceptable — clonr has no multi-user concurrent session requirement at v1.

### CSRF stance

**No CSRF token required** for the session cookie layer.

`SameSite=Strict` means the browser will not send the session cookie on any cross-site navigation, form submission, or fetch — including `<form>` POSTs from attacker-controlled origins. This eliminates the classical CSRF vector entirely. A CSRF token would be defense-in-depth against a buggy browser that ignores `SameSite`, but no mainstream browser has shipped such a regression since 2021. The additional implementation and UX complexity (per-form tokens, AJAX header handling) is not justified.

Exception to revisit: if clonr ever serves responses with `Access-Control-Allow-Origin: *` or relaxes CORS policy, this stance must be re-evaluated. Current policy is no CORS headers — browser fetch from any origin that is not the clonr UI origin will fail pre-flight.

### Dev mode

`CLONR_AUTH_DEV_MODE=1` bypasses both auth sources (cookie and Bearer). This env var must be absent or `0` in any non-local deployment. Gilfoyle controls this gate.

---

## Migration path

| Phase | What ships | Who |
|---|---|---|
| **Now** | `localStorage` modal, `Authorization: Bearer` on every fetch | Dinesh a64bee0a (done) |
| **Next** | `POST /api/v1/auth/login`, `POST /api/v1/auth/logout`, session cookie middleware, sliding re-sign | Dinesh (new instance, implements from this ADR) |
| **After** | Login page replaces modal; `localStorage` key removed from UI entirely | Dinesh (follow-on PR) |

The Bearer path remains functional throughout. CLI and scripted API consumers are unaffected at every phase.

---

## OIDC v1.1 composition

When OIDC is enabled (ADR-0001 v1.1), the middleware gains a third auth source: `Authorization: Bearer <oidc-jwt>`. The OIDC JWT is validated against the configured issuer's JWKS endpoint and mapped to `scope=admin`. The session cookie layer composes cleanly — an operator can log in via the UI using either an OIDC-issued token (if the UI implements OIDC redirect flow) or a local admin API key; both produce the same `AuthContext` and the same session cookie. The initramfs node-scope path remains API-key-only in both v1.0 and v1.1.

---

## Consequences

- No regression to ADR-0001. API key primitive is unchanged.
- No server-side session table. No migration required beyond adding `CLONR_SESSION_SECRET` to the deployment env.
- `CLONR_SESSION_SECRET` rotation is the hard revocation mechanism — document this clearly in ops runbook.
- SameSite=Strict cookie requires the UI to be served from the same origin as the API. This is already the case (clonr-serverd serves the SPA). If the UI is ever split to a separate origin, this ADR must be revisited.
- The 30-minute slide re-sign adds one cookie write per ~30 min of activity. Negligible on any target deployment.
