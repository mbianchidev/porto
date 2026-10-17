---
name: Porto
description: A dense, neutral-graphite desktop control desk for supervising local apps, containers, clusters, and virtual machines
colors:
  canvas-dark: "#141516"
  panel-dark-theme: "#1b1c1e"
  panel-raised-dark-theme: "#222326"
  line-dark-theme: "#2b2d31"
  ink-dark-theme: "#e7e8ea"
  canvas-light: "#f3f4f5"
  panel-light-theme: "#ffffff"
  line-light-theme: "#e1e3e6"
  ink-light-theme: "#17181a"
  instrument: "#0f1011"
  olive-signal-dark: "#8fa04f"
  olive-signal-light: "#64703a"
  amber-signal-dark: "#d58d33"
  amber-signal-light: "#bd7725"
  fault-red-dark: "#cf5a4c"
  fault-red-light: "#b0443a"
typography:
  display:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'Segoe UI', 'Helvetica Neue', Inter, Roboto, Arial, sans-serif"
    fontSize: "clamp(1.45rem, 2vw, 1.8rem)"
    fontWeight: 700
    lineHeight: 1.1
    letterSpacing: "-0.02em"
  headline:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'Segoe UI', 'Helvetica Neue', Inter, Roboto, Arial, sans-serif"
    fontSize: "17px"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.01em"
  body:
    fontFamily: "-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'Segoe UI', 'Helvetica Neue', Inter, Roboto, Arial, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.45
    letterSpacing: "normal"
  label:
    fontFamily: "ui-monospace, 'SF Mono', 'SFMono-Regular', Menlo, Monaco, Consolas, monospace"
    fontSize: "10-12px"
    fontWeight: 400
    lineHeight: 1.2
    letterSpacing: "0.05em"
rounded:
  sm: "4px"
  md: "6px"
  lg: "8px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  lg: "20px"
  xl: "32px"
---

# Design System: Porto

## Overview

**Creative North Star: "The Control Desk"**

Porto is one dense, professional control desk for local apps, containers,
images, builds, storage, networks, Kubernetes, and VMs. It uses a fixed
instrument rail, fused fleet signal and control strips, ranked inventories,
and a docked inspector. The palette is neutral graphite with three operational
signal colors: olive, amber, and fault red. Porto deliberately avoids the
identity accents of other desktop container tools; no pink, purple, or bright
brand blue appears in the interface.

The story order is deliberate: choose a workload family from the rail, read its
fleet health, find the affected resource in the ranked inventory, act through
visible controls, then inspect routing, runtime details, logs, terminals, files,
metrics, or maintenance without losing the list.

**Key characteristics:**
- Dark theme by default; a light theme and "Match system" are available in
  **Settings > Theme**.
- Neutral graphite (dark) or cool off-white (light) working surfaces. No warm
  beige, putty, or paper tones.
- Instrument surfaces (navigation rail, fleet signal rail, drawer readouts,
  command strip, log console, terminals) stay dark in both themes.
- Status meaning is carried by exactly three signal colors.
- Monospace, uppercase, letter-spaced labels for machine-reported values.
- Hairline borders separate regions; shadows are soft and subtle.

## Themes

The theme is stored with the other experience preferences (`theme`: `dark`,
`light`, or `system`; default `dark`). `public/experience-preferences.js`
applies `html[data-theme]` before React renders so there is no flash, and the
preferences provider keeps it in sync, following `prefers-color-scheme` when
the theme is `system`. Every color in `App.css` is a token from `index.css`;
theme-specific tokens live under `:root[data-theme="dark"]` and
`:root[data-theme="light"]`, while instrument tokens are shared.

## Colors

### Signals (both themes)
- **Olive** (dark `#8fa04f` / light `#64703a`): healthy, running, ready.
- **Amber** (dark `#d58d33` / light `#bd7725`): in progress, attention, and the
  universal keyboard focus outline.
- **Fault red** (dark `#cf5a4c` / light `#b0443a`): crashed, error,
  destructive.

Each signal has `-soft` (tinted background) and `-text` (readable foreground)
variants per theme.

### Neutrals
- **Canvas**: dark `#141516`, light `#f3f4f5`.
- **Panel / raised / hover / selected**: dark `#1b1c1e` / `#222326` /
  `#27292c` / `#2c2e32`; light `#ffffff` / `#ffffff` / `#f4f5f6` / `#eceef0`.
- **Fields**: dark `#101112`, light `#ffffff`.
- **Lines**: dark `#2b2d31` / `#3b3e43`; light `#e1e3e6` / `#c7cacf`.
- **Ink**: dark `#e7e8ea` / `#a6a9ae` / `#7b7f86`; light `#17181a` / `#4d5057`
  / `#71757c`.
- **Instrument**: `#0f1011` rail and readouts, `#0c0d0e` consoles.

### Named rules
**The Three-Signal Rule.** Only olive, amber, and fault red carry operational
meaning. Links use a desaturated slate (`--link`) so they never compete with
signals.

**The Neutral Rule.** Working surfaces stay neutral gray. Never introduce warm
beige or a brand-identity hue as a surface or accent color.

## Typography

**UI font:** the platform system stack (`-apple-system`, SF Pro, Segoe UI,
Helvetica Neue, Inter, Roboto).
**Label/mono font:** `ui-monospace`, SF Mono, Menlo, Consolas.

### Hierarchy
- **Display** (700, `clamp(1.45rem, 2vw, 1.8rem)`): page `h1` only.
- **Headline** (600, 17px): panel and settings section headings.
- **Body** (400, 14px base; 12–13px in dense rows).
- **Label** (mono, 10–12px, uppercase, `0.05em`): machine-reported values such
  as state, ports, PIDs, branches, timestamps, and commands.

## Layout

The shell uses a fixed 240px instrument rail and a fluid main area padded
`22px 32px 48px`. A resource screen is a single column until something is
selected; then `.workArea:has(> .inspector)` adds an inspector column of
`--inspector-width` (`clamp(400px, 32vw, 560px)`), so creation and detail
panels such as **New cluster** have room for their forms. The fleet signal rail
and control bar are fused into one instrument. Control bars wrap their actions
instead of overflowing.

Inventories are joined tables: one bordered container whose rows are separated
by hairlines, highlight on hover, and mark the selected row with a tinted
background and a 2px inset bar.

**Responsive behavior:**
- **≤1080px:** inventory columns compress; the inspector narrows to
  `min(400px, 44vw)`.
- **≤860px:** the rail becomes an off-canvas panel over a scrim, inspectors
  become full-screen detail surfaces, and the work area is one column.
- **≤620px:** control bar items take the full width; secondary machine facts
  hide and touch targets grow.

## Elevation and shape

Surfaces are separated by 1px borders, not shadows. Resting buttons use
`--shadow-sm`; menus, the expanded channel, and the maximized inspector use
`--shadow-md`; tooltips and popovers use `--shadow-pop`. Hard offset "printed"
shadows are not used.

Radii are 4px (`--radius-sm`: chips, small controls), 6px (`--radius-md`:
buttons, inputs, rows), and 8px (`--radius-lg`: panels, control bar,
inspector). Status lamps and the brand-mark dots are the only circles.

## Components

### Buttons
13px/500 sans, 6px radius, `--line-strong` border on `--panel-raised`.
Hover raises to `--panel-hover` with `--line-hover`; active uses
`--panel-selected`. Focus-visible is a 3px amber outline with a 2px offset.
Disabled is 0.48 opacity. Icon buttons keep their role colors (olive, amber,
red, slate), a visible tooltip, an `aria-label`, and visually hidden text.

### Inputs
`--field` background, `--line-strong` border, 6px radius. Focus darkens the
border and adds the olive underline glow.

### Navigation rail
Dark in every theme. Sections use native-button toggles with chevrons and
`aria-expanded`. The active route gets a subtle light fill and a neutral inset
marker.

### Status lamp
A 9px LED-style circle: olive running, amber starting, red crashed or error,
gray stopped. It is always paired with a text state.

### Integration status
Provider and tool readiness rows show a 2px inset left bar in the signal color
(olive ready, amber working or missing, red error).

## Do's and don'ts

### Do
- Keep dark as the default theme and every color tokenized.
- Keep instrument surfaces dark in both themes.
- Reserve olive, amber, and fault red for health, progress, and fault.
- Keep the inventory visible while the inspector is open.
- Give every icon-only control an accessible name and a tooltip.

### Don't
- Use warm beige, putty, or paper backgrounds, or a ruled background texture.
- Use pink, purple, or bright brand blue accents.
- Use hard offset shadows or radii above 8px.
- Add decorative or looping motion; reveal transitions stay around
  150–220ms and are disabled under reduced motion.
- Hide state behind an icon or a color alone.
