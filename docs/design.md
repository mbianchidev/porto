---
name: Porto
description: A dense, engraved process-control board for supervising local and self-hosted projects
colors:
  graphite-ink: "#222521"
  graphite-soft: "#4c514b"
  graphite-faint: "#70756d"
  putty-canvas: "#c8c2b4"
  panel-putty: "#e9e5d9"
  panel-raised: "#f4f0e5"
  panel-graphite: "#252925"
  line-putty: "#8c897e"
  line-graphite: "#343832"
  olive-signal: "#64703a"
  olive-soft: "#d5d9b8"
  amber-signal: "#bd7725"
  amber-soft: "#f1d3a8"
  fault-red: "#a43e32"
  fault-red-soft: "#ebc0b8"
  panel-white: "#fffdf7"
typography:
  display:
    fontFamily: "'Avenir Next', Avenir, 'Helvetica Neue', Arial, sans-serif"
    fontSize: "clamp(1.75rem, 3vw, 2.6rem)"
    fontWeight: 700
    lineHeight: 1.05
    letterSpacing: "-0.035em"
  headline:
    fontFamily: "'Avenir Next', Avenir, 'Helvetica Neue', Arial, sans-serif"
    fontSize: "18px"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.02em"
  title:
    fontFamily: "ui-monospace, 'SFMono-Regular', Menlo, Monaco, Consolas, monospace"
    fontSize: "12px"
    fontWeight: 700
    lineHeight: 1
    letterSpacing: "0.05em"
  body:
    fontFamily: "'Avenir Next', Avenir, 'Helvetica Neue', Arial, sans-serif"
    fontSize: "15px"
    fontWeight: 400
    lineHeight: 1.45
    letterSpacing: "normal"
  label:
    fontFamily: "ui-monospace, 'SFMono-Regular', Menlo, Monaco, Consolas, monospace"
    fontSize: "9px"
    fontWeight: 400
    lineHeight: 1.2
    letterSpacing: "0.06em"
rounded:
  sm: "2px"
  md: "3px"
  lg: "4px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "13px"
  lg: "24px"
components:
  button-primary:
    backgroundColor: "{colors.panel-raised}"
    textColor: "{colors.graphite-ink}"
    typography: "{typography.body}"
    rounded: "{rounded.sm}"
    padding: "7px 10px"
  button-primary-hover:
    backgroundColor: "{colors.panel-white}"
    textColor: "{colors.graphite-ink}"
  icon-button:
    backgroundColor: "transparent"
    textColor: "{colors.graphite-ink}"
    rounded: "{rounded.sm}"
    width: "31px"
    height: "31px"
  input-field:
    backgroundColor: "{colors.panel-raised}"
    textColor: "{colors.graphite-ink}"
    typography: "{typography.label}"
    rounded: "{rounded.sm}"
    padding: "8px 9px"
  nav-link:
    backgroundColor: "transparent"
    textColor: "#adafa5"
    padding: "7px 10px"
  nav-link-active:
    backgroundColor: "#ddd9cd"
    textColor: "{colors.graphite-ink}"
---

# Design System: Porto

## Overview

**Creative North Star: "The Broadcast Patchbay"**

Porto reads as a piece of painted-metal broadcast equipment, not a web dashboard: a fleet signal rail, engraved channel labels, physical-feeling status lamps, and a service drawer that racks open in place. The thesis is one dense process-control board, not a collection of cards — every project is a compact channel strip in a fixed, ranked stack, and the first viewport is a dark control header, a fleet signal rail, a filter strip, and ranked channels with no oversized summary blocks. The palette is putty, graphite, olive, amber, and fault red: warm neutral metal tones carrying three status colors that never wander from their operational meaning.

The story order is deliberate: read fleet health first (the fleet rail's four lamp counters), find the affected project (status lamp + name in the channel row), act immediately (four always-visible quick actions per channel), then expand in place for branch, routing, maintenance, and logs. Nothing about the layout is decorative — gaps, radii, and shadows all read as hardware fit-and-finish rather than software polish.

**Key Characteristics:**
- Dense, single-column ranked channel stack; no card grid, no oversized hero or summary tiles.
- Dark graphite control surfaces (header, fleet rail, log console) bracket putty/panel-toned working surfaces.
- Status meaning is carried by exactly three signal colors: olive (healthy), amber (in-progress/attention), fault red (crashed/destructive).
- Monospace, uppercase, letter-spaced labels everywhere data or state is reported — the "engraved label" language.
- Flat hardware surfaces with hard-offset "printed" shadows at rest; soft ambient shadows are reserved for things that lift off the surface (header bar, expanded channel, open menus, tooltips).

## Colors

The palette is a warm, desaturated putty-and-graphite metal system with three narrow, non-decorative signal colors layered on top.

### Primary
- **Olive Signal** (`#64703a`): the "healthy/running" signal — status lamps, active filter underline, focus ring on toggles, Sendbox/cleanup icon glyphs, branch-menu selection highlight (via `--olive-soft`).

### Secondary
- **Amber Signal** (`#bd7725`): the "in-progress/attention" signal — starting-state lamps, fleet attention message, keyboard focus outlines (all interactive elements), setup icon glyph, missing/unsupported integration status.

### Tertiary
- **Fault Red** (`#a43e32`): the "fault/destructive" signal — crashed/error lamps and state text, error banner, destructive quick actions (kill, remove, error status).

### Neutral
- **Graphite Ink** (`#222521`): primary text and heading color on light surfaces.
- **Graphite Soft** (`#4c514b`): secondary body text (subtitles, descriptions).
- **Graphite Faint** (`#70756d`): tertiary/meta text (placeholders, faint labels).
- **Putty Canvas** (`#c8c2b4`): the page background, under a 24px horizontal ruled-line texture.
- **Panel Putty** (`#e9e5d9`): base panel/card surface.
- **Panel Raised** (`#f4f0e5`): lighter "raised" surface for inputs, buttons, and hover states.
- **Panel Graphite** (`#252925`): dark control surfaces — app header, fleet rail, drawer readouts, command strip, log console.
- **Line Putty** (`#8c897e`) / **Line Graphite** (`#343832`): border colors for light-surface hairlines and their stronger/emphasized counterpart.
- **Panel White** (`#fffdf7`): text on dark surfaces and button hover background.

### Named Rules
**The Three-Signal Rule.** Only olive, amber, and fault red carry operational meaning (healthy, in-progress, fault). No other hue is introduced for status; a link uses a desaturated blue-gray (`#334d63`) precisely so it never competes with the signal vocabulary.

**The Metal-Not-Paper Rule.** All panel surfaces stay inside the putty→panel-raised→panel-graphite family. Pure white (`--white` / Panel White) is reserved for text-on-dark and transient hover states, never a resting page or panel background.

## Typography

**Body/UI Font:** 'Avenir Next', Avenir, 'Helvetica Neue', Arial, sans-serif
**Label/Mono Font:** ui-monospace, 'SFMono-Regular', Menlo, Monaco, Consolas, monospace

**Character:** A humanist sans for page titles, prose, and primary identity strings, paired with a monospace face for everything that reads as a machine-reported value — state, ports, PIDs, branches, timestamps, commands. The pairing is what makes the "control board" read: prose looks written, data looks measured.

### Hierarchy
- **Display** (700, `clamp(1.75rem, 3vw, 2.6rem)`, 1.05, `-0.035em`): page-level `h1` in the page intro only.
- **Headline** (600, 18px, 1.2, `-0.02em`): drawer title ("[Project] service channel") and settings section intros (20px variant).
- **Title** (700, 12px, uppercase, `0.05em`, mono): drawer panel headers, console header title — small-caps-style section labels, not prose headings.
- **Body** (400, 15px base / 12–13px in dense contexts, 1.45): running body copy, channel identity name, drawer paragraph text.
- **Label** (400, 9–11px, uppercase, `0.05–0.08em`, mono): the pervasive "engraved" metadata language — fleet datum labels, channel `<small>` captions, `dt` terms, log timestamps, button labels inside strips.

### Named Rules
**The Engraved Label Rule.** Any text describing a machine fact (branch, port, PID, state, route, timestamp, command) is rendered in uppercase, letter-spaced mono at 9–11px. Prose and proper names stay in sans. Mixing the two inside one string is the signal that something is a live readout, not copy.

## Layout

The page is a single centered column (`min(1480px, calc(100% - 28px))`) with a tight top/bottom rhythm (14px top, 40px bottom) — there is no side rail, no dashboard grid of cards. Vertical rhythm between major regions (header, banners, page intro, fleet rail + control bar, channel board) runs 8–13px; internal component padding runs 5–13px. Nothing exceeds ~24px of gap anywhere in the system — density is the point.

The fleet signal rail and the filter/control bar are visually fused into one control unit: the rail has bottom corners squared to 0 and the control bar's top border is removed, so together they read as a single instrument panel with two rows rather than two separate cards. Project channels stack in a single-column `channelStack` (4px gaps); multi-branch projects are wrapped in a bordered `projectGroup.multi` container with a small header, so a source project with several branch instances still reads as one physical unit with sub-channels, not a scattered list.

**Responsive behavior:**
- **≤1080px:** channel columns compress (branch/route columns narrow); the two-column drawer grid collapses to one column.
- **≤860px:** header and page intro stack vertically; fleet attention message and the route column hide; control bar drops to a 2-column grid with search spanning full width; hygiene/integration panels go single-column.
- **≤620px:** full mobile stack — header controls, primary nav (equal-width tabs), fleet rail, control bar, and channel rows all collapse to their narrowest form; the branch/port datum column hides entirely (state column takes its place); quick-action icon buttons grow to 40×40px for touch; command strip and log line columns re-flow to stacked/narrower grids.

### Named Rules
**The Fused Instrument Rule.** The fleet rail and control bar never separate into two visually distinct cards — they are one header instrument with a dark reporting row on top and a lighter control row beneath.

## Elevation & Depth

Porto is a hybrid: flat "painted metal" surfaces at rest, with a hard, small-offset, unblurred "printed" shadow used almost everywhere (e.g. `1px 2px 0 rgba(52,56,50,0.32)` on buttons, `2px 3px 0 rgba(52,56,50,0.2–0.26)` on channels and panels) — the effect of a silkscreened panel edge, not a floating card. Soft, blurred ambient shadows are reserved for elements that genuinely lift off the surface: the app header, an expanded project channel, the open branch menu, and icon-button tooltips.

### Shadow Vocabulary
- **printed-flat** (`1px 2px 0 rgba(52, 56, 50, 0.2–0.35)`): resting buttons, panels, notices, drawer sub-panels. The default state for nearly every bordered surface.
- **printed-raised** (`2px 3px 0 rgba(...)` to `3px 4px 0 rgba(...)`): channels, control bar, app header — slightly heavier offset for surfaces that sit "above" the canvas.
- **ambient-lift** (`3px 4px 12px rgba(45,43,36,0.24)` header; `3px 5px 12px rgba(52,48,38,0.24)` expanded channel): soft ambient glow for elements that are actively "on" or opened.
- **ambient-float** (`4px 7px 18px rgba(45,43,36,0.34)` branch menu; `3px 5px 14px rgba(32,31,27,0.35)` tooltip): overlay-level shadow for anything that floats above the layout (popovers, tooltips).

### Named Rules
**The Printed Edge Rule.** Flat surfaces at rest use a hard, unblurred offset shadow (a "printed" edge), never a soft blur. Soft blurred shadows are earned only by elements that pop open or float: the header, an expanded drawer, a dropdown, a tooltip.

## Shapes

Corners stay tight and mechanical: 2px (buttons, inputs, lamps' hairline borders), 3px (channels, panels, notices, the control bar), and 4px only on the app header — nothing in the system uses a soft/large radius. Borders are 1px solid, drawn from the putty/graphite border tokens, and are the primary way regions are demarcated (backgrounds shift subtly; borders do the separating). Status lamps and the header's three brand-mark dots are the only circular shapes, deliberately reading as physical indicator LEDs rather than UI dots.

### Named Rules
**The Sharp Metal Rule.** No radius in the system exceeds 4px. A rounder corner reads as software chrome, not painted equipment, and breaks the world.

## Components

### Buttons
- **Shape:** 2px radius, 1px solid `line-graphite` border.
- **Primary (default `<button>`):** `panel-raised` background, `graphite-ink` text, 600-weight 11px sans, `7px 10px` padding, printed-flat shadow.
- **Hover:** border darkens to near-black, background shifts to `panel-white`.
- **Focus-visible:** 3px solid `amber-signal` outline, 2px offset — the one universal focus treatment across buttons, links, inputs, and the channel toggle.
- **Active:** shadow removed and the button nudges `translate(1px, 1px)` — a literal "pressed switch" micro-interaction.
- **Disabled:** 0.48 opacity, `not-allowed` cursor.
- **Icon buttons (quick actions / maintenance bar):** 31×31px (40×40px on mobile), transparent background, no border chrome; the glyph itself carries color by role (setup: warm brown `#6a4418`; logs: blue-gray `#334d63`; sendbox/cleanup: olive `#53612f`; kill/remove/destructive: fault red `#812e26`). Every icon button carries a visible tooltip (dark graphite popover, `data-tooltip` driven) plus an `aria-label` and visually-hidden text label — never icon-only with no accessible name.

### Inputs / Fields
- **Style:** 1px `line-putty` border, 2px radius, `panel-raised` background, mono label typography for search/branch fields.
- **Focus:** border darkens to `line-graphite` and an inset 2px `olive-signal` underline glow appears (`box-shadow: inset 0 -2px 0 var(--olive)`) — the same "underline lights up" treatment used on the search bar and status filter chips.
- **Combobox (branch picker):** search input with an inline leading icon and chevron; results are pinned (default branch, then `main`, then `master`) ahead of the remaining options sorted alphabetically; selected/hovered rows highlight in `olive-soft`.

### Navigation
- **Primary nav:** a dark pill-strip (`#191c19`) of ghost links; the active route gets a light putty background (`#ddd9cd`) with a small hard-offset shadow, everything else is transparent text that brightens to white on hover. Focus uses the universal amber outline.

### Status Lamp (signature component)
A 9px circle with a 1px dark border and an inset highlight, reading as a physical panel LED. Color encodes state directly: olive (running), amber (starting), fault red (crashed/error), mid-gray (stopped/unset). It appears at fleet level (aggregate counts) and per-channel (leading the channel row) — it is always paired with a text state label, never used as the sole signal.

### Project Channel (signature component)
The core unit of the system: a bordered, printed-flat row (`channelFace`) with a 6-column grid — lamp, identity, branch datum, route datum, state, chevron — plus a strip of always-visible quick actions. Clicking the row toggles a `serviceDrawer` open beneath it in place (never a modal or navigation), revealing dark-graphite runtime readouts (PID/port/strategy), two side-by-side panels for routing/branch controls, a dark command-strip showing the literal launch command, a maintenance bar with less-common/destructive actions, and an inline log console. Multiple branch instances of one source project are visually grouped under a shared `projectGroupHeader` inside a bordered "multi" wrapper.

## Do's and Don'ts

### Do:
- **Do** keep the fleet rail and control bar fused as one instrument (no gap, rail's bottom radius squared into the bar's top).
- **Do** reserve olive/amber/fault-red exclusively for health/progress/fault meaning; use the desaturated blue-gray for links instead of introducing a fourth signal hue.
- **Do** use hard, unblurred "printed" offset shadows on resting surfaces; reserve soft ambient shadows for elements that open, lift, or float (header, expanded drawer, menus, tooltips).
- **Do** render machine-reported values (branch, port, PID, state, timestamps, commands) in uppercase, letter-spaced mono; keep prose and proper names in sans.
- **Do** expand project detail inline in a `serviceDrawer` beneath its channel row, never in a modal, side panel, or separate route.
- **Do** give every icon-only control an `aria-label`, a visible tooltip, and visually-hidden text.

### Don't:
- **Don't** turn project channels into a card grid with generous whitespace — the system is a dense, single-column, fixed-rank stack.
- **Don't** use a border radius greater than 4px anywhere; rounder corners break the painted-metal/mechanical read.
- **Don't** add decorative or looping motion; the only animation is a ~150–220ms reveal/rotate on state changes (banner-in, drawer-reveal, chevron rotation, tooltip fade), all disabled under `prefers-reduced-motion`.
- **Don't** hide a project's state behind an icon alone — the status lamp always pairs with a text state label.
- **Don't** introduce a fourth accent color for status; if a new state is needed, map it onto olive/amber/fault-red rather than adding a hue.
