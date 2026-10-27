# Sky Styling Guide

A framework-agnostic description of the visual language used by the Sky frontend. The source uses Tailwind + shadcn/ui + Radix; this guide translates those classNames into raw CSS values so an agent writing plain HTML can reproduce the look.

Read this top-to-bottom for the design language, or jump to a section to copy a specific recipe.

---

## 1. Foundation

Sky is **dark-mode-only in practice** — `<body>` always carries `class="dark"`. Build for the dark palette by default.

### 1.1 Colors (resolved)

The shadcn tokens (HSL, from `globals.css` `.dark`):

| Token | HSL | Hex (approx) | Used for |
|---|---|---|---|
| `--background` | `224 71.4% 4.1%` | `#030712` | page background, drawer/dialog body |
| `--foreground` | `210 20% 98%` | `#f8fafc` | primary text |
| `--card` | `224 71.4% 4.1%` | `#030712` | card surface |
| `--popover` | `224 71.4% 4.1%` | `#030712` | tooltip/popover surface |
| `--primary` | `210 20% 98%` | `#f8fafc` | default button background, switch-on |
| `--primary-foreground` | `220.9 39.3% 11%` | `#111827` | text on primary |
| `--secondary` / `--muted` / `--accent` | `215 27.9% 16.9%` | `#1e293b` | subdued surface, switch-off, ghost-hover |
| `--muted-foreground` | `217.9 10.6% 64.9%` | `#94a3b8` | secondary text |
| `--destructive` | `0 62.8% 30.6%` | `#7f1d1d` | destructive button |
| `--border` / `--input` | `215 27.9% 16.9%` | `#1e293b` | default borders |
| `--ring` | `216 12.2% 83.9%` | `#cbd5e1` | focus ring |
| `--radius` | `0.5rem` | `8px` | base radius |

The named (Tailwind) colors that appear hardcoded all over the codebase. Memorize the gray ramp and the cyan/violet/pink trio — those are the entire identity.

```css
/* Grays (the chrome) */
--gray-300:#d1d5db; --gray-400:#9ca3af; --gray-500:#6b7280;
--gray-600:#4b5563; --gray-700:#374151; --gray-800:#1f2937;
--gray-900:#111827; --gray-950:#030712;

/* Primary accent (cyan) — active state, links, focus, "playing" */
--cyan-300:#67e8f9; --cyan-400:#22d3ee; --cyan-500:#06b6d4;
--cyan-600:#0891b2; --cyan-700:#0e7490; --cyan-900:#164e63;

/* Secondary accent (violet/purple) — pairs with cyan in gradients */
--violet-400:#a78bfa; --violet-500:#8b5cf6;
--purple-400:#c084fc; --purple-500:#a855f7; --purple-600:#9333ea;

/* Tertiary accent (pink) — third color in chart trios, rank #3 */
--pink-400:#f472b6; --pink-500:#ec4899;

/* Status colors */
--green-400:#4ade80;  /* "Liked" */
--green-500:#22c55e;  /* online, Spotify */
--green-600:#16a34a;
--emerald-400:#34d399;
--amber-400:#fbbf24;  /* rank #4, degraded */
--red-400:#f87171; --red-500:#ef4444;  /* destructive, error */
--yellow-500:#eab308;  /* warning */
```

**Opacity suffixes (Tailwind `/30`, `/50`, `/80`).** When you see e.g. `bg-cyan-500/20` in source, that's `rgba(6,182,212,0.20)`. Always translate `<color>/<NN>` to alpha = `NN/100`. The codebase uses these all the time — surfaces are rarely opaque grays; they're tinted accent colors at 10–30% over a near-black backdrop, which is what gives the UI its "glow."

### 1.2 The signature surface

Almost every panel, card, and chart container in Sky uses the same recipe. If in doubt, default to this:

```css
.surface {
  background: rgba(17, 24, 39, 0.30);     /* gray-900 @ 30% */
  border: 1px solid #1f2937;              /* gray-800 */
  border-radius: 8px;
  padding: 16px;                          /* p-4 */
}
```

For elevated/floating panels (sidebar, top bar, floating button), add **backdrop blur** and increase the alpha:

```css
.surface--floating {
  background: rgba(3, 7, 18, 0.80);       /* gray-950 @ 80% */
  backdrop-filter: blur(8px);             /* backdrop-blur-sm */
  -webkit-backdrop-filter: blur(8px);
  border: 1px solid #1f2937;
}
```

### 1.3 Typography

- **Font family:** Inter (loaded via `next/font`), fall back to system sans. The TopBar and one-off legacy components fall back to `Arial, Helvetica, sans-serif` — that is the only place a non-Inter family appears.
- **`font-mono`** is used for any number-y display (timestamps, latency, play counts, percentages, leaderboard ranks). Use `ui-monospace, SFMono-Regular, Menlo, Monaco, "Cascadia Mono", monospace`.

Type scale (Tailwind→px):

| Class | Size | Line | Common use |
|---|---|---|---|
| `text-[9px]` / `text-[10px]` | 9–10px | — | chart axis ticks, heatmap labels |
| `text-xs` | 12px | 16 | tooltips, captions, secondary metadata |
| `text-sm` | 14px | 20 | body text, table rows, button labels |
| `text-base` | 16px | 24 | mobile inputs (16px to disable iOS zoom) |
| `text-lg` | 18px | 28 | section titles, sheet/dialog titles |
| `text-2xl` | 24px | 32 | page-level hero titles |

Weights: `font-light` (300), `font-medium` (500), `font-semibold` (600), `font-extralight` (200, used by the `TypewriterTitle`).

Section labels in charts and panels use a uniform treatment:
```css
.section-label {
  font-size: 12px;            /* text-xs */
  color: #6b7280;             /* gray-500 */
  text-transform: uppercase;
  letter-spacing: 0.05em;     /* tracking-wider */
}
```

### 1.4 Spacing rhythm

Tailwind's 4px scale. Common values used in Sky:

| Token | Value |
|---|---|
| `gap-1` / `space-y-1` | 4px |
| `gap-2` / `p-2` | 8px |
| `gap-3` / `p-3` | 12px |
| `p-4` | 16px (most container padding) |
| `p-6` | 24px (cards, dialog content) |
| `space-y-6` | 24px (form vertical rhythm in drawers) |

### 1.5 Radii

Three sizes, all derived from `--radius: 8px`:
- `rounded-sm` → 2px (heatmap cells, tiny chips)
- `rounded-md` → 6px (buttons, inputs, tabs, focus rings)
- `rounded-lg` → 8px (cards, dialogs, drawers, panels)
- `rounded-full` → 9999px (badges, switch thumb, avatars, floating button)

### 1.6 Shadows

- Card / form-control: `shadow-sm` → `0 1px 2px 0 rgb(0 0 0 / 0.05)`
- Sheet / dialog: `shadow-lg` → `0 10px 15px -3px rgb(0 0 0 / 0.1), 0 4px 6px -4px rgb(0 0 0 / 0.1)`
- Floating button: `shadow-2xl` → `0 25px 50px -12px rgb(0 0 0 / 0.25)`
- **Colored glows** (used on accent buttons): `box-shadow: 0 10px 15px -3px rgba(6,182,212,0.20)` — i.e. `shadow-cyan-500/20`. These are what make the cyan/green CTA buttons feel "lit."

### 1.7 Transitions

Everything interactive uses `transition: <prop> 150ms` (Tailwind default `transition-colors`) or `200ms` (`duration-200`). Sheets use `300ms` close / `500ms` open. Floating-button hovers use `300ms`. Pulse/orb animations use multi-second cubic-bezier loops.

```css
.transition-colors { transition: background-color 150ms, color 150ms, border-color 150ms; }
.transition-all-200 { transition: all 200ms; }
```

### 1.8 Breakpoints

```
phone:  <640px         (custom variant — applies via .device-phone wrapper)
tablet: 640–1024px     (.device-tablet wrapper)
sm:     ≥640px         (Tailwind default)
md:     ≥768px
lg:     ≥1024px
```

The codebase commonly does `phone:hidden` (hide on phone) and `phone:px-4 phone:pt-2` (compact mobile spacing). For HTML output, replicate with `@media (max-width: 639px)`.

---

## 2. Primitives — raw CSS recipes

These are direct translations of the shadcn/ui sources used in the codebase.

### 2.1 Button

Base + size + variant compose into one class. Default variant is `default` + size `default`.

```css
.btn {
  display: inline-flex; align-items: center; justify-content: center;
  gap: 8px; white-space: nowrap;
  border-radius: 6px;
  font-size: 14px; font-weight: 500;
  transition: background-color 150ms, color 150ms, border-color 150ms;
  cursor: pointer;
}
.btn:focus-visible {
  outline: none;
  box-shadow: 0 0 0 2px #030712, 0 0 0 4px #cbd5e1;  /* offset + ring */
}
.btn:disabled { pointer-events: none; opacity: 0.5; }
.btn svg { width: 16px; height: 16px; flex-shrink: 0; pointer-events: none; }

/* Sizes */
.btn--default { height: 40px; padding: 8px 16px; }
.btn--sm      { height: 36px; padding: 0 12px; border-radius: 6px; }
.btn--lg      { height: 44px; padding: 0 32px; border-radius: 6px; }
.btn--icon    { height: 40px; width: 40px; padding: 0; }

/* Variants */
.btn--primary     { background: #f8fafc; color: #111827; }
.btn--primary:hover     { background: rgba(248,250,252,0.9); }
.btn--destructive { background: #7f1d1d; color: #f8fafc; }
.btn--destructive:hover { background: rgba(127,29,29,0.9); }
.btn--outline {
  border: 1px solid #1e293b; background: #030712; color: #f8fafc;
}
.btn--outline:hover { background: #1e293b; }
.btn--secondary { background: #1e293b; color: #f8fafc; }
.btn--secondary:hover { background: rgba(30,41,59,0.8); }
.btn--ghost { background: transparent; color: #f8fafc; }
.btn--ghost:hover { background: #1e293b; }
.btn--link { color: #f8fafc; text-underline-offset: 4px; }
.btn--link:hover { text-decoration: underline; }
```

**Signature CTA buttons** (gradient + colored glow) — used on Connect Spotify, "Continue", playback's circular play button:

```css
.btn--cta-cyan {
  background: linear-gradient(to right, #0891b2, #9333ea);
  color: white; font-weight: 600;
  box-shadow: 0 10px 15px -3px rgba(6,182,212,0.20);
}
.btn--cta-cyan:hover {
  background: linear-gradient(to right, #06b6d4, #a855f7);
}
.btn--cta-spotify {
  background: linear-gradient(to right, #16a34a, #22c55e);
  color: white; font-weight: 600;
  box-shadow: 0 10px 15px -3px rgba(34,197,94,0.20);
}
```

### 2.2 Input

```css
.input {
  display: flex; height: 40px; width: 100%;
  border-radius: 6px;
  border: 1px solid #1e293b;
  background: #030712;
  padding: 8px 12px;
  font-size: 16px;            /* prevents iOS zoom */
  color: #f8fafc;
}
.input::placeholder { color: #94a3b8; }
.input:focus-visible {
  outline: none;
  box-shadow: 0 0 0 2px #030712, 0 0 0 4px #cbd5e1;
}
@media (min-width: 768px) { .input { font-size: 14px; } }
```

### 2.3 Switch (toggle)

```css
.switch {
  position: relative;
  display: inline-flex; align-items: center;
  height: 24px; width: 44px;
  flex-shrink: 0; cursor: pointer;
  border-radius: 9999px;
  border: 2px solid transparent;
  background: #1e293b;        /* unchecked */
  transition: background-color 150ms;
}
.switch[data-checked="true"] { background: #f8fafc; }
.switch__thumb {
  display: block; height: 20px; width: 20px;
  border-radius: 9999px;
  background: #030712;
  box-shadow: 0 10px 15px -3px rgb(0 0 0 / 0.10);
  transform: translateX(0);
  transition: transform 150ms;
}
.switch[data-checked="true"] .switch__thumb { transform: translateX(20px); }
```

### 2.4 Badge

```css
.badge {
  display: inline-flex; align-items: center;
  border-radius: 9999px;
  border: 1px solid transparent;
  padding: 2px 10px;
  font-size: 12px; font-weight: 600;
  transition: background-color 150ms;
}
.badge--default     { background: #f8fafc; color: #111827; }
.badge--secondary   { background: #1e293b; color: #f8fafc; }
.badge--destructive { background: #7f1d1d; color: #f8fafc; }
.badge--outline     { color: #f8fafc; border-color: #1e293b; background: transparent; }

/* Common ad-hoc accent badges seen throughout the app: */
.badge--cyan    { background: rgba(6,182,212,0.20); color: #22d3ee; border-color: rgba(6,182,212,0.30); }
.badge--green   { background: rgba(34,197,94,0.20); color: #4ade80; border-color: rgba(34,197,94,0.30); }
.badge--red     { background: rgba(239,68,68,0.20); color: #f87171; border-color: rgba(239,68,68,0.30); }
.badge--gray    { background: rgba(107,114,128,0.20); color: #9ca3af; border-color: rgba(107,114,128,0.30); }
```

### 2.5 Card

```css
.card {
  border-radius: 8px;
  border: 1px solid #1e293b;
  background: #030712;            /* var(--card) */
  color: #f8fafc;
  box-shadow: 0 1px 2px 0 rgb(0 0 0 / 0.05);
}
.card__header { display: flex; flex-direction: column; gap: 6px; padding: 24px; }
.card__title  { font-size: 24px; font-weight: 600; line-height: 1; letter-spacing: -0.025em; }
.card__desc   { font-size: 14px; color: #94a3b8; }
.card__content { padding: 0 24px 24px; }
.card__footer  { display: flex; align-items: center; padding: 0 24px 24px; }
```

### 2.6 Tooltip

```css
.tooltip {
  z-index: 50; overflow: hidden;
  border-radius: 6px; border: 1px solid #1e293b;
  background: #030712;             /* popover */
  padding: 6px 12px;
  font-size: 14px; color: #f8fafc;
  box-shadow: 0 10px 15px -3px rgb(0 0 0 / 0.10);
  /* enter: fade in 0 → 1, scale 0.95 → 1; exit: reverse */
  animation: tooltip-in 150ms ease-out;
}
@keyframes tooltip-in {
  from { opacity: 0; transform: scale(0.95); }
  to   { opacity: 1; transform: scale(1); }
}
```

### 2.7 Dialog (modal)

Centered, max-width 512px (`max-w-lg`), grid layout with 16px gap:

```css
.dialog__overlay {
  position: fixed; inset: 0; z-index: 50;
  background: rgba(0,0,0,0.40);
  animation: fade-in 200ms ease-out;
}
.dialog__content {
  position: fixed; left: 50%; top: 50%; z-index: 50;
  display: grid; gap: 16px;
  width: 100%; max-width: 512px;
  transform: translate(-50%, -50%);
  border: 1px solid #1e293b;
  background: #030712;
  padding: 24px;
  border-radius: 8px;
  box-shadow: 0 10px 15px -3px rgb(0 0 0 / 0.10);
  animation: dialog-in 200ms ease-out;
}
@keyframes fade-in { from{opacity:0} to{opacity:1} }
@keyframes dialog-in {
  from { opacity: 0; transform: translate(-50%, -48%) scale(0.95); }
  to   { opacity: 1; transform: translate(-50%, -50%) scale(1); }
}
.dialog__header { display: flex; flex-direction: column; gap: 6px; }
.dialog__title { font-size: 18px; font-weight: 600; line-height: 1; letter-spacing: -0.025em; }
.dialog__desc  { font-size: 14px; color: #94a3b8; }
.dialog__footer { display: flex; flex-direction: column-reverse; gap: 8px; }
@media (min-width: 640px) {
  .dialog__footer { flex-direction: row; justify-content: flex-end; }
}
```

The close affordance is an `X` icon (16×16) absolutely positioned at `top: 16px; right: 16px;` with `opacity: 0.7` rising to `1` on hover.

### 2.8 Sheet (drawer)

Slides from any side. Right side is the default; left side is used for the services sidebar.

```css
.sheet__overlay {
  position: fixed; inset: 0; z-index: 50;
  background: rgba(0,0,0,0.80);             /* darker than dialog */
  animation: fade-in 300ms ease-out;
}
.sheet--right {
  position: fixed; top: 0; right: 0; z-index: 50;
  height: 100%; width: 75%; max-width: 384px; /* 24rem on sm+ */
  border-left: 1px solid #1e293b;
  background: #030712; padding: 24px;
  box-shadow: 0 10px 15px -3px rgb(0 0 0 / 0.10);
  animation: slide-in-right 500ms cubic-bezier(0.32, 0.72, 0, 1);
}
.sheet--left {
  position: fixed; top: 0; left: 0; z-index: 50;
  height: 100%; width: 75%; max-width: 384px;
  border-right: 1px solid #1e293b;
  background: #030712; padding: 24px;
  animation: slide-in-left 500ms cubic-bezier(0.32, 0.72, 0, 1);
}
@keyframes slide-in-right { from { transform: translateX(100%); } to { transform: translateX(0); } }
@keyframes slide-in-left  { from { transform: translateX(-100%); } to { transform: translateX(0); } }
/* On close: 300ms ease-in slide-out in the same axis */

.sheet__header { display: flex; flex-direction: column; gap: 8px; }
.sheet__title  { font-size: 18px; font-weight: 600; color: #f8fafc; }
.sheet__desc   { font-size: 14px; color: #94a3b8; }
.sheet__footer {
  display: flex; flex-direction: column-reverse; gap: 8px;
}
@media (min-width: 640px) {
  .sheet__footer { flex-direction: row; justify-content: flex-end; }
}
```

**Phone full-screen variant.** When `device-phone` is detected, sheets become full-screen edge-to-edge: `inset: 0; width: 100%; height: 100%; max-width: none; border: 0; border-radius: 0; padding: 0;`.

### 2.9 Collapsible

A pure layout primitive — a trigger row with a chevron icon (rotate 180° when open), and a content region that renders only when open. Sky uses `<ChevronDown>` collapsed and `<ChevronUp>` expanded; both 16×16. No height animation — content snaps in/out.

### 2.10 Toaster

Sonner toaster, mounted at root with `theme="dark"` and `position="bottom-right"`. Toasts inherit the dark surface treatment automatically.

---

## 3. Chrome — top-level layout components

### 3.1 TopBar (`top-bar.tsx`)

Sticky 20px-tall bar at the top of the desktop layout. Hidden on phone.

```css
.topbar {
  position: sticky; top: 0; z-index: 40;
  width: 100%;
  border-bottom: 1px solid #1f2937;          /* gray-800 */
  background: rgba(3, 7, 18, 0.80);          /* gray-950/80 */
  backdrop-filter: blur(4px);
  -webkit-backdrop-filter: blur(4px);
}
.topbar__inner {
  margin: 0 auto; max-width: 1400px;          /* container */
  padding: 0 8px;
  height: 20px;
  display: flex; align-items: center; justify-content: space-between;
  font-size: 12px; color: #9ca3af;            /* gray-400 */
  white-space: nowrap;
}
.topbar__group { display: flex; align-items: center; gap: 16px; }
.topbar__cell  { display: flex; align-items: center; }
.topbar__cell svg { width: 12px; height: 12px; margin-right: 4px; }

/* Latency color coding */
.latency--good   { color: #22c55e; }   /* < 100ms  */
.latency--ok     { color: #eab308; }   /* < 300ms  */
.latency--bad    { color: #ef4444; }   /* ≥ 300ms  */
.latency--unknown{ color: #6b7280; }
```

The original uses **container queries** (`@container`, `@[400px]:inline`, `@[500px]:flex`, `@[650px]:hidden`) to hide cells progressively as the inner container narrows. In plain HTML, swap to media queries on viewport width — most pages are full-width, so `@media (max-width: 500px)` ≈ `@[500px]:hidden`.

### 3.2 FloatingMenuButton (`FloatingMenuButton.tsx`)

A 56px round button anchored bottom-left, with a glowing ambient orb inside. Hidden on phone; the orb itself is the brand mark.

```css
.fab {
  position: fixed; left: 24px; bottom: 24px; z-index: 50;
}
.fab__halo {
  position: absolute; inset: 0; border-radius: 9999px;
  filter: blur(24px); opacity: 0.30;
  background: radial-gradient(circle, rgba(6,182,212,0.8) 0%, rgba(139,92,246,0.6) 50%, transparent 70%);
  transition: opacity 500ms;
}
.fab:hover .fab__halo { opacity: 0.50; }
.fab__btn {
  position: relative;
  height: 56px; width: 56px;
  border-radius: 9999px;
  border: 1px solid rgba(55,65,81,0.30);     /* gray-700/30 */
  background: rgba(3,7,18,0.90);
  backdrop-filter: blur(4px);
  box-shadow: 0 25px 50px -12px rgb(0 0 0 / 0.25);
  transition: all 300ms;
  display: flex; align-items: center; justify-content: center;
  cursor: pointer;
}
.fab__btn:hover {
  background: rgba(17,24,39,0.95);
  border-color: rgba(6,182,212,0.40);
  transform: scale(1.05);
}
```

#### The Ambient Orb

A 32×32 stack of nine concentric layers. Reproduce literally — every layer matters.

```html
<div class="orb">
  <!-- 1: outer aura -->
  <div class="orb__aura"></div>
  <!-- 2: outermost slow ring -->
  <div class="orb__ring orb__ring--slow"></div>
  <!-- 3: outer rotating ring -->
  <div class="orb__ring orb__ring--outer"></div>
  <!-- 4: counter-rotating middle ring -->
  <div class="orb__ring orb__ring--mid"></div>
  <!-- 5: fast inner ring -->
  <div class="orb__ring orb__ring--fast"></div>
  <!-- 6: gradient sphere body -->
  <div class="orb__sphere"></div>
  <!-- 7: core energy pulse -->
  <div class="orb__core"></div>
  <!-- 8: secondary pulse layer -->
  <div class="orb__core orb__core--alt"></div>
  <!-- 9-11: orbiting particles + 12-13: specular highlights -->
  <div class="orb__particle orb__particle--1"><span></span></div>
  <div class="orb__particle orb__particle--2"><span></span></div>
  <div class="orb__particle orb__particle--3"><span></span></div>
  <div class="orb__highlight orb__highlight--primary"></div>
  <div class="orb__highlight orb__highlight--secondary"></div>
</div>
```

```css
.orb { position: relative; height: 32px; width: 32px; }
.orb > * { position: absolute; }

.orb__aura {
  inset: -8px; border-radius: 9999px;
  filter: blur(16px); opacity: 0.40;
  background: radial-gradient(circle, rgba(6,182,212,0.7) 0%, rgba(139,92,246,0.5) 40%, transparent 70%);
  animation: pulse 4s cubic-bezier(0.4,0,0.6,1) infinite;
}
.orb__ring { inset: 0; border-radius: 9999px; }
.orb__ring--slow {
  inset: -2px;
  background: conic-gradient(from 0deg, transparent 0%, rgba(6,182,212,0.3) 10%, transparent 20%, transparent 80%, rgba(139,92,246,0.3) 90%, transparent 100%);
  animation: spin 20s linear infinite;
}
.orb__ring--outer {
  background: conic-gradient(from 0deg, transparent 0%, rgba(6,182,212,0.7) 15%, transparent 30%, transparent 50%, rgba(139,92,246,0.7) 65%, transparent 80%);
  animation: spin 10s linear infinite;
}
.orb__ring--mid {
  inset: 3px;
  background: conic-gradient(from 90deg, transparent 0%, rgba(236,72,153,0.5) 12%, transparent 25%, rgba(6,182,212,0.5) 37%, transparent 50%, rgba(139,92,246,0.5) 62%, transparent 75%, rgba(6,182,212,0.4) 87%, transparent 100%);
  animation: spin 7s linear infinite reverse;
}
.orb__ring--fast {
  inset: 6px;
  background: conic-gradient(from 45deg, transparent 0%, rgba(255,255,255,0.4) 5%, transparent 10%, transparent 45%, rgba(6,182,212,0.6) 50%, transparent 55%, transparent 90%, rgba(139,92,246,0.5) 95%, transparent 100%);
  animation: spin 4s linear infinite;
}
.orb__sphere {
  inset: 8px; border-radius: 9999px;
  background: radial-gradient(circle at 30% 30%, rgba(255,255,255,0.2) 0%, rgba(6,182,212,0.9) 25%, rgba(139,92,246,0.95) 55%, rgba(88,28,135,0.9) 100%);
  box-shadow: inset 0 0 10px rgba(6,182,212,0.6), inset 0 0 4px rgba(255,255,255,0.3);
}
.orb__core {
  inset: 11px; border-radius: 9999px;
  background: radial-gradient(circle, rgba(255,255,255,0.9) 0%, rgba(6,182,212,0.7) 30%, transparent 70%);
  animation: pulse 2s cubic-bezier(0.4,0,0.6,1) infinite;
}
.orb__core--alt {
  inset: 10px;
  background: radial-gradient(circle, transparent 0%, rgba(139,92,246,0.3) 50%, transparent 70%);
  animation: pulse 2.5s cubic-bezier(0.4,0,0.6,1) infinite 0.5s;
}
.orb__particle { inset: 0; transform-origin: center; animation: spin 3s linear infinite; }
.orb__particle--2 { inset: 2px; animation: spin 5s linear infinite reverse; }
.orb__particle--3 { inset: 4px; animation-duration: 4s; }
.orb__particle > span {
  position: absolute; top: 0; left: 50%; transform: translateX(-50%);
  height: 4px; width: 4px; border-radius: 9999px;
  background: rgba(34,211,238,0.8); filter: blur(0.5px);
}
.orb__particle--2 > span { top: auto; bottom: 0; background: rgba(192,132,252,0.7); }
.orb__particle--3 > span { top: 50%; left: auto; right: 0; transform: translateY(-50%); height: 2px; width: 2px; background: rgba(244,114,182,0.6); filter: none; }
.orb__highlight--primary {
  top: 5px; left: 6px; height: 8px; width: 8px; border-radius: 9999px;
  background: radial-gradient(circle, rgba(255,255,255,0.8) 0%, transparent 70%);
}
.orb__highlight--secondary {
  bottom: 7px; right: 6px; height: 4px; width: 4px; border-radius: 9999px;
  background: rgba(255,255,255,0.25); filter: blur(0.5px);
}

@keyframes spin  { to { transform: rotate(360deg); } }
@keyframes pulse {
  0%,100% { opacity: 1; }
  50%     { opacity: 0.5; }
}
```

### 3.3 ServicesSidebar (left sheet)

Width 320px (mobile) / 380px (sm+), full-height, `bg-gray-950/95` with `backdrop-blur-sm`. Header → scroll area of service items → fixed-bottom user button.

Each **service item** is the canonical Sky list-row and reusable for any "row of clickable things":

```html
<button class="svc-item">
  <div class="svc-item__icon-wrap">
    <div class="svc-item__icon"><!-- 20×20 lucide icon, color cyan-400 --></div>
    <div class="svc-item__status svc-item__status--online"></div>
  </div>
  <div class="svc-item__body">
    <div class="svc-item__name">Music</div>
    <div class="svc-item__desc">Music playback and management</div>
  </div>
  <div class="svc-item__status-text">online</div>
</button>
```

```css
.svc-item {
  width: 100%; padding: 12px; text-align: left; cursor: pointer;
  border-radius: 8px;
  border: 1px solid #1f2937;
  background: rgba(17,24,39,0.50);
  display: flex; align-items: center; gap: 12px;
  transition: all 200ms;
}
.svc-item:hover { background: rgba(31,41,55,0.70); border-color: #374151; }
.svc-item__icon-wrap { position: relative; flex-shrink: 0; }
.svc-item__icon {
  padding: 8px; border-radius: 8px; background: rgba(31,41,55,0.80); color: #22d3ee;
  transition: background 150ms;
}
.svc-item:hover .svc-item__icon { background: rgba(55,65,81,0.80); }
.svc-item__status {
  position: absolute; bottom: -2px; right: -2px;
  height: 10px; width: 10px; border-radius: 9999px;
  border: 2px solid #111827;
}
.svc-item__status--online   { background: #22c55e; }
.svc-item__status--degraded { background: #eab308; }
.svc-item__status--offline  { background: #6b7280; }
.svc-item__status--unknown  { background: #4b5563; }
.svc-item__body { flex: 1; min-width: 0; }
.svc-item__name {
  font-size: 14px; font-weight: 500; color: #e5e7eb;
  text-overflow: ellipsis; overflow: hidden; white-space: nowrap;
  transition: color 150ms;
}
.svc-item:hover .svc-item__name { color: #67e8f9; }
.svc-item__desc { font-size: 12px; color: #6b7280; text-overflow: ellipsis; overflow: hidden; white-space: nowrap; }
.svc-item__status-text { font-size: 12px; color: #4b5563; text-transform: capitalize; flex-shrink: 0; }
```

The **bottom user button** uses a circular avatar with the cyan→purple gradient — this gradient is one of Sky's most-repeated motifs:

```css
.avatar--gradient {
  height: 32px; width: 32px; border-radius: 9999px;
  background: linear-gradient(to bottom right, #06b6d4, #9333ea);
  display: flex; align-items: center; justify-content: center;
  color: white; font-size: 14px; font-weight: 500;
}
```

### 3.4 TypewriterTitle

A glowing cyan title that reveals via a width-mask animation. Use for hero/loading text.

```css
.tw-title { font-weight: 200; position: relative; }
.tw-title__placeholder { letter-spacing: 0.3em; opacity: 0; }  /* reserves layout */
.tw-title__reveal {
  position: absolute; inset: 0; overflow: hidden;
  /* width is animated 0 → 100% over `speed` ms */
}
.tw-title__reveal span {
  letter-spacing: 0.3em; white-space: nowrap;
  color: #16f5f7; opacity: 0.75;
  text-shadow: 0 0 10px rgba(22,245,247,0.3), 0 0 20px rgba(22,245,247,0.1);
}
.tw-title__cursor {
  position: absolute; top: 0; height: 100%; width: 8px;
  background: linear-gradient(90deg, transparent 0%, rgba(22,245,247,0.8) 50%, transparent 100%);
  box-shadow: 0 0 12px rgba(22,245,247,0.6), 0 0 24px rgba(22,245,247,0.3);
  filter: blur(1px);
  animation: pulse 2s cubic-bezier(0.4,0,0.6,1) infinite;
  /* `left` tracks the reveal width */
}
```

Note the unusual cyan `#16f5f7` here — slightly more saturated than the standard `cyan-400 #22d3ee`. Use it only for this glow effect.

### 3.5 Safe areas (native iOS/Android)

When wrapping for Capacitor, the page reserves the iOS notch:

```css
:root { --safe-top: 0px; --safe-bottom: 0px; }
.native.platform-ios { --safe-top: env(safe-area-inset-top, 0px); --safe-bottom: env(safe-area-inset-bottom, 0px); }
.native.platform-ios.device-phone .page-content { padding-top: var(--safe-top); }
```

Sticky headers add `padding-top: calc(var(--safe-top, 0px) + 8px)` and footers add `padding-bottom: calc(var(--safe-bottom, 0px) + 12px)`. For HTML output that runs in a browser, set both to `0px` and you're fine.

---

## 4. Composition recipe — the Action Detail drawer

This is the canonical **right-side configuration sheet** pattern (`ActionDetailDrawer.tsx`). Reuse it whenever you need a settings/edit drawer.

### 4.1 Skeleton

```html
<aside class="sheet sheet--right ad-drawer">
  <header class="ad-drawer__header">
    <div class="ad-drawer__title-row">
      <h2 class="sheet__title">Edit Action</h2>
      <!-- mobile-only delete X -->
    </div>
    <p class="sheet__desc">Modify the action settings and triggers</p>
  </header>

  <div class="ad-drawer__scroll">
    <section class="form-row">
      <label class="label">Name</label>
      <input class="input" placeholder="My Action" />
    </section>

    <section class="form-row">
      <label class="label">Prompt</label>
      <div class="prompt-row">
        <div class="prompt-row__preview">No prompt set</div>
        <button class="btn btn--outline btn--sm">+ Add Prompt</button>
      </div>
    </section>

    <section class="form-row">
      <label class="label"><svg .../> MCP Servers</label>
      <div class="badge-cluster">
        <span class="badge badge--cyan">Gmail</span>
        <span class="badge badge--outline">Calendar</span>
      </div>
    </section>

    <div class="toggle-row">
      <div class="toggle-row__label">
        <label>Human in the Loop</label>
        <button class="info-tip" aria-label="info">?</button>
      </div>
      <!-- .switch -->
    </div>

    <details class="advanced">
      <summary class="advanced__trigger">
        <span><svg .../> Advanced Settings</span>
        <svg class="chev" .../>
      </summary>
      <div class="advanced__body">…fields…</div>
    </details>
  </div>

  <footer class="ad-drawer__footer">
    <button class="btn btn--destructive">Delete</button>
    <div class="spacer"></div>
    <button class="btn btn--outline">Cancel</button>
    <button class="btn btn--primary">Save</button>
  </footer>
</aside>
```

### 4.2 Drawer-specific CSS

```css
.ad-drawer {
  width: 500px; max-width: 500px;
  display: flex; flex-direction: column;
  overflow: hidden; padding: 0;
}
.ad-drawer__header  { flex-shrink: 0; padding: 24px 24px 0; }
.ad-drawer__scroll  { flex: 1; overflow-y: auto; padding: 0 24px; }
.ad-drawer__scroll > * + * { margin-top: 24px; }   /* space-y-6 */
.ad-drawer__footer  {
  flex-shrink: 0; display: flex; gap: 8px; padding: 16px 24px;
}
.ad-drawer__footer .spacer { flex: 1; }

/* Phone full-screen variant */
@media (max-width: 639px) {
  .ad-drawer {
    inset: 0; width: 100%; max-width: none; border: 0; border-radius: 0;
  }
  .ad-drawer__header { padding: 8px 16px; border-bottom: 1px solid #1f2937; }
  .ad-drawer__scroll { padding: 0 16px; }
  .ad-drawer__footer { padding: 12px 16px; border-top: 1px solid #1f2937; background: #030712; }
}

.label {
  font-size: 14px; font-weight: 500; color: #f8fafc;
  display: inline-flex; align-items: center; gap: 8px;
}
.form-row { display: flex; flex-direction: column; gap: 8px; }
.prompt-row { display: flex; align-items: center; gap: 8px; }
.prompt-row__preview {
  flex: 1; font-size: 14px; color: #9ca3af;
  text-overflow: ellipsis; overflow: hidden; white-space: nowrap;
}
.badge-cluster { display: flex; flex-wrap: wrap; gap: 8px; }
.toggle-row { display: flex; align-items: center; justify-content: space-between; }
.toggle-row__label { display: flex; align-items: center; gap: 6px; }
```

The "Advanced Settings" disclosure uses native `<details>` for plain HTML; in the React source it's a Radix Collapsible. Either gives the same visual.

---

## 5. Chart & data-viz styling

The Music page is the largest concentration of data viz. Match these patterns whenever rendering analytics in a Sky-styled doc.

### 5.1 Chart palette

Sky's chart colors are **always** drawn from this five-color palette in this order:

```
1. cyan-400   #22d3ee   (primary)
2. violet-400 #a78bfa   (secondary)
3. pink-400   #f472b6   (tertiary)
4. green-400  #4ade80   (quaternary)
5. amber-400  #fbbf24   (rank #4)
6. emerald-400 #34d399  (rank #5, fallback)
```

Series get filled at **strong opacity for stroke** (≈100%) and **gradient fill 60% → 10%** for area.

### 5.2 Container

Every chart sits in the signature surface, padding 16px, with an optional title row:

```html
<section class="chart-card">
  <header class="chart-card__head">
    <span class="section-label">Listening Rhythm</span>
    <span class="chart-card__total">12h <small>past 7 days</small></span>
  </header>
  <div class="chart-card__viz" style="height: 112px;"><!-- chart --></div>
  <footer class="chart-card__legend">
    <span><i style="background:#22d3ee"></i>Playlists</span>
    <span><i style="background:#a78bfa"></i>Albums</span>
    <span><i style="background:#f472b6"></i>Artists</span>
    <span><i style="background:#4ade80"></i>Liked</span>
  </footer>
</section>
```

```css
.chart-card { background: rgba(17,24,39,0.30); border: 1px solid #1f2937; border-radius: 8px; padding: 16px; }
.chart-card__head { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: 8px; }
.chart-card__total { font-family: ui-monospace,Menlo,monospace; font-size: 18px; color: #f8fafc; }
.chart-card__total small { font-size: 12px; color: #6b7280; margin-left: 8px; }
.chart-card__legend { display: flex; gap: 12px; margin-top: 8px; flex-wrap: wrap; font-size: 12px; color: #9ca3af; }
.chart-card__legend i {
  display: inline-block; width: 8px; height: 8px; border-radius: 9999px; margin-right: 6px;
  vertical-align: middle;
}
```

### 5.3 Stacked area (ListeningRhythmChart)

- **Type:** monotone stacked area, `height: 112px`.
- **Fills:** linear gradient top→bottom, `from rgba(<color>,0.6)` to `rgba(<color>,0.10)`. The "Liked" series is dimmer: `0.4` → `0.05`.
- **Strokes:** the same color at full opacity, 1–1.5px width.
- **Axes:** Y axis hidden. X axis ticks at `font-size: 9px`, `fill: #4b5563` (gray-600). Show only first/last labels.
- **Grid:** none.
- **Tooltip:** floating popover with the dot indicator — reuse `.tooltip` styling, list series with their colored dots.

For raw SVG you can produce the same look by rendering `<path>` for each layer, with `<linearGradient id="g-cyan" x1=0 y1=0 x2=0 y2=1>` and stops at the alpha values above.

### 5.4 Donut (ContextBreakdown)

- 112×112 (`w-28 h-28`) flex item, alongside a vertical legend.
- `innerRadius: 30`, `outerRadius: 50`, `paddingAngle: 2`, `stroke: none`.
- Segments take colors from the chart palette in series order.
- Legend: stacked rows `gap: 6px`, each `text-sm color: #d1d5db`, with the percentage in `font-mono` matching the segment color.

### 5.5 Heatmap (ListeningHeatmap)

7×24 grid, day rows × hour columns. **Cells are 12px tall, ≥10px wide, gap 2px, radius 2px**, colored by intensity:

```css
.heatmap__cell { height: 12px; min-width: 10px; flex: 1; border-radius: 2px; transition: background 150ms; }
.heatmap__cell--0    { background: rgba(31,41,55,0.50); }   /* gray-800/50 */
.heatmap__cell--low  { background: rgba(22,78,99,0.60); }   /* cyan-900/60 */
.heatmap__cell--med  { background: rgba(14,116,144,0.70); } /* cyan-700/70 */
.heatmap__cell--high { background: rgba(6,182,212,0.80); }  /* cyan-500/80 */
.heatmap__cell--max  { background: #22d3ee; }               /* cyan-400 */
```

Day/hour labels: `font-size: 10px` (or `9px` for hours), `font-family: monospace`, `color: #4b5563`. Render hour labels every 4 hours only. Add a tiny legend row at the bottom: "Less" → 5 graduated boxes → "More".

### 5.6 Terminal-style horizontal bar (TopArtistsLeaderboard)

The "Sky aesthetic" rendered as a leaderboard. Each row:

```
1  [img]  Artist Name              ████████████ 12h 34m   2,341 plays
2  [img]  Another Artist           ████████ 9h 12m         1,889 plays
```

- Rank: `font-mono`, 16px-wide right-aligned column, colored:  cyan/violet/pink/amber/emerald for ranks 1–5; gray-500 thereafter.
- Avatar: 32×32 round with `bg-gray-800` placeholder.
- Bar: a row of `█` (U+2588) Unicode blocks, count proportional to `value/maxValue * BAR_LENGTH` (12–20 chars). Color matches the rank color.
- Time: `font-mono text-xs color: #6b7280`.
- Plays: `font-mono text-xs color: #9ca3af` with `" plays"` suffix in `color: #4b5563`.

Use this pattern any time you want a leaderboard or ranked list — it reads as native to Sky.

### 5.7 NowPlayingCard

```css
.now-playing {
  display: flex; gap: 16px; padding: 16px;
  border: 1px solid #1f2937; border-radius: 8px;
  background: rgba(17,24,39,0.30);
}
.now-playing__art {
  position: relative; width: 64px; height: 64px;
  border-radius: 8px; overflow: hidden;
  border: 1px solid rgba(55,65,81,0.50);
}
.now-playing__art--playing { box-shadow: 0 0 0 2px rgba(6,182,212,0.30); }
.now-playing__art-glow {
  position: absolute; inset: 0; filter: blur(24px); opacity: 0.40;
  /* background-image: url(albumArt); */
}
.now-playing__title { font-size: 14px; font-weight: 600; color: white; }
.now-playing__sub   { font-size: 12px; color: #9ca3af; }
.now-playing__time  { font-family: ui-monospace,monospace; font-size: 12px; color: #6b7280; }
.play-button {
  height: 40px; width: 40px; border-radius: 9999px;
  background: linear-gradient(to bottom right, #06b6d4, #9333ea);
  color: white; display: flex; align-items: center; justify-content: center;
}
.play-button:hover { background: linear-gradient(to bottom right, #22d3ee, #a855f7); }
/* Mobile: 48×48 */
```

The progress bar uses a custom-styled slider where the **filled** portion is `linear-gradient(to right, #06b6d4, #a855f7)` and the **thumb** is `#22d3ee`.

### 5.8 Tracking row (history feed, library)

The reusable Sky list-row variant for "media-with-thumbnail":

```css
.media-row {
  display: flex; align-items: center; gap: 12px;
  padding: 8px; border-radius: 4px;
  transition: background 150ms;
}
.media-row:hover { background: rgba(31,41,55,0.30); }
.media-row__thumb {
  width: 48px; height: 48px; border-radius: 4px; overflow: hidden;
  background: #1f2937; flex-shrink: 0;
}
.media-row__title { font-size: 14px; color: white; }
.media-row:hover .media-row__title { color: #22d3ee; }
.media-row__sub { font-size: 12px; color: #6b7280; }
```

Mobile long-text rows fade the trailing edge to suggest overflow:
```css
.fade-mask::after {
  content: ""; position: absolute; inset: 0 0 0 auto; width: 24px;
  background: linear-gradient(to left, #030712, transparent);
}
```

---

## 6. Animation reference (keyframes)

Define these once and reuse — they cover almost every motion in the codebase.

```css
@keyframes fade-in     { from{opacity:0} to{opacity:1} }
@keyframes fade-out    { from{opacity:1} to{opacity:0} }
@keyframes spin        { to { transform: rotate(360deg); } }
@keyframes pulse       { 0%,100%{opacity:1} 50%{opacity:0.5} }
@keyframes pulse-slow  { 0%,100%{opacity:1} 50%{opacity:0.5} }   /* 5s duration */
@keyframes ping        { 75%,100% { transform: scale(2); opacity: 0; } }
@keyframes slide-in-right { from{transform:translateX(100%)} to{transform:translateX(0)} }
@keyframes slide-in-left  { from{transform:translateX(-100%)} to{transform:translateX(0)} }
@keyframes slide-in-top   { from{transform:translateY(-100%)} to{transform:translateY(0)} }
@keyframes slide-in-bottom{ from{transform:translateY(100%)}  to{transform:translateY(0)} }
@keyframes zoom-in-95     { from{transform:scale(0.95);opacity:0} to{transform:scale(1);opacity:1} }
```

Loaders use a 16×16 `lucide:Loader2` rotating `animate-spin` (1s linear infinite) — color usually `#94a3b8` or matches the surrounding text.

---

## 7. Composition principles (the "vibe")

These are the rules to follow when you're inventing something new and want it to look like Sky:

1. **Backdrop-first.** Almost every panel sits on `rgba(17,24,39,0.30)` over the near-black body. Avoid solid mid-grays.
2. **Borders are gray-800, not black.** Use `#1f2937` or `#1e293b` at full opacity, never plain `#333`.
3. **Cyan is the primary accent.** Active states, focus rings, hover-text-color, "playing now," progress fills — always cyan-400 (`#22d3ee`) or cyan-500 (`#06b6d4`). Don't introduce blue.
4. **Cyan→Purple gradient is the brand.** Use it on hero CTAs, the play button, avatars, the floating menu orb. Direction: usually `to right` or `to bottom right`.
5. **Mono for numbers, sans for prose.** Any displayed metric — durations, latency, percentages, ranks, play counts — uses `font-mono`.
6. **Section labels are uppercase tracked.** `font-size: 12px; color: #6b7280; text-transform: uppercase; letter-spacing: 0.05em;` — that's it. Don't bold them.
7. **Tinted alpha over solid.** Status pills are `bg-<color>/20 text-<color>-400 border-<color>/30`, never solid color. This applies to badges, alerts, chip-style filters.
8. **Hover is a color shift, not a transform.** Color/background transitions in 150ms. Reserve `scale(1.05)` for the floating button only.
9. **Empty states are calm.** "No connected servers." → `font-size: 14px; color: #6b7280`. No illustrations, no big icons.
10. **Charts breathe.** Always wrap viz in the signature surface with 16px padding and a label header. Never render bare on the page background.

---

## 8. Quick reference — minimal CSS reset to drop into a fresh `.html`

```css
:root {
  --bg: #030712; --fg: #f8fafc;
  --surface: rgba(17,24,39,0.30);
  --border: #1f2937;
  --muted: #94a3b8; --muted-2: #6b7280;
  --accent: #22d3ee; --accent-2: #06b6d4;
  --violet: #a78bfa; --pink: #f472b6;
  --radius: 8px;
}
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; height: 100%; }
body {
  background: var(--bg); color: var(--fg);
  font-family: Inter, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  font-size: 14px; line-height: 1.4;
  -webkit-font-smoothing: antialiased;
}
code, pre, .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, monospace; }
```

Drop that in, then pick recipes from §2–5. The result will read as native Sky.
