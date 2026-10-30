---
version: 1
slug: "web-templates-users-html"
primary_target: "web/templates/users.html"
related_targets: ["web/templates/admin_login.html"]
---

## Surface

Admin dashboard: `web/templates/users.html` and `web/templates/admin_login.html`. Mode: Operate. Visitors are administrators performing short account-management tasks on desktop or phone.

## Direction contract

THESIS: A quiet operations console. It refuses the generic blue SaaS dashboard and replaces it with a flat graphite workspace where the task and account state are the only things asking for attention.

OWN-WORLD: Green-tinted graphite surfaces, pale mint text, a single aqua accent for primary actions and live state, hairline borders, restrained 12–14px radii, system sans, tabular numerals, and no gradients. Elevation is expressed by one border, not border plus shadow.

STORY: The administrator opens the page, understands server and account state at a glance, finds a user, and changes access or credentials without hunting through decoration.

FIRST VIEWPORT: A compact brand bar sits left with theme, language, and logout controls right. Below it, one horizontal overview strip carries users, active, blocked, and rate-limit-exempt counts. The global rate-limit control follows, then the user list with search. On a phone the overview becomes a 2×2 strip, controls wrap, and each user row becomes a labeled card with full-width actions.

FORM: Server-rendered HTML/CSS in the existing `html/template` files. No JavaScript framework, no external assets, no image generation. Motion is limited to state transitions under 200ms. seed: none (unavailable)

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance.
