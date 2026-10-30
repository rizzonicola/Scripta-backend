# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Existing Go server with `html/template` templates embedded into the binary. No client-side framework, no external CSS or font assets. Admin pages are server-rendered and protected by a signed HttpOnly cookie.

## Users

A single administrator manages a personal Scripta notes server. The admin may use a desktop browser for routine work and a phone browser for quick checks or account changes.

## Product Purpose

Scripta Notes Server provides the sync API and account storage behind the Scripta Flutter app. Its admin dashboard lets the operator inspect the server, manage users, control access, reset passwords, and tune the sync rate limit without touching the database or API directly.

## Positioning

The dashboard is a private operations surface attached to a self-hosted notes system. It favors direct, low-ceremony account controls over broad analytics or multi-tenant administration.

## Operating Context

The dashboard is opened occasionally, not used all day. Tasks are short: find a user, block an account, reset a password, delete an account, or toggle the rate limit. It should remain usable on a narrow phone viewport and in low-attention contexts.

## Capabilities and Constraints

- Form-based admin login using `ADMIN_USER` and `ADMIN_PASS`.
- Search users by username or ID.
- Create users, block/unblock accounts, reset passwords, delete accounts.
- Toggle the global per-user sync rate limit and exempt individual users.
- Display account counts, status badges, creation dates, and server dependency information.
- Language switcher: Italian, English, French, plus system preference.
- Theme switcher: system, light, dark.
- Templates must remain self-contained and compatible with the existing CSP nonce. No external CDN, webfont, or image dependency.
- Existing HTTP routes, form field names, flash codes, and security behavior must not change.

## Brand Commitments

The admin surface should feel simple, quiet, and elegant. The requested general palette is dark gray with an aqua/teal accent. Gradients should be avoided. The interface should stay legible and compact rather than decorative.

## Evidence on Hand

- `web/templates/admin_login.html` and `web/templates/users.html` are the current visual implementation.
- `README.md` documents the admin routes, security model, and user-management behavior.
- The existing tests exercise login, search, flash handling, blocking, rate limits, password reset, and deletion.

## Product Principles

- Keep the task visible: account actions and server state come before decoration.
- Preserve earned familiarity: standard forms, tables, buttons, and confirmation patterns.
- Prefer flat surfaces, hairline borders, and a single accent over layered effects.
- Mobile is a first-class way to perform a quick account action, not a reduced desktop view.
- Do not change product behavior while changing its appearance.

## Accessibility & Inclusion

Use semantic HTML, keyboard-visible focus states, readable contrast, touch targets of at least 40px, and responsive layouts that do not require horizontal scrolling. Respect `prefers-reduced-motion`.

## Assumptions

- This is a redesign of the existing admin surface, not a new product with additional capabilities.
- The primary audience is one technical operator; no analytics, billing, or multi-admin roles are implied.
- The user's requested dark-gray/aqua palette is a binding visual constraint; a light theme remains available for users who explicitly select it.
