---
name: kam-manuals-style
description: 'Use when creating, editing, styling, or reviewing KAM HTML manuals in manuals/, including installation guides, API references, architecture documentation, and user guides. Defines documentation layout, typography, colors, writing, accessibility, and validation.'
---

# KAM Manuals Style Guide

Version: 3.1

## Direction

Use a dark **Apple-inspired editorial** identity: quiet charcoal surfaces, precise typography, generous whitespace, and understated detail. Take inspiration from Apple's product documentation and macOS interface craftsmanship while retaining KAM's own identity. Use KAM branding, not Apple logos, imagery, or product-page layouts.

The result should feel refined, calm, and dependable. Documentation is an operator's working surface, not a marketing page. Keep technical information dense enough to scan and comfortable enough to read at length.

KAM is reliable, lightweight, secure, radio-native, automation-focused, and extensible. Always assume a beginner-to-novice audience, including readers new to radio networks, command-line tools, and software administration.

## Audience

- Start with the purpose and a concrete example before describing internals. Explain what each component does and why a reader needs it.
- Define unfamiliar terms and expand abbreviations on first use, including MeshCore, LoRa, KISS, serial port, channel, SQLite, YAML, and API. A glossary supplements these explanations; it does not replace them.
- Introduce concepts in dependency order. Distinguish the host computer, radio hardware, radio firmware, and KAM software before explaining message flows.
- State prerequisites and where commands run. Explain configuration values, required substitutions, expected results, and how to recover from common errors.
- Use short, plain-language paragraphs and concrete examples. Keep accurate technical terms, but explain them without assuming programming or networking experience.
- Keep advanced implementation details in a clearly labelled reference section after the introductory material. Every manual, including an API or architecture reference, must provide beginner context.
- Separate current capabilities, limitations, and planned features explicitly. Do not imply guaranteed delivery, privacy, or safety-critical reliability.

Use neutral near-black backgrounds, soft white text, subtle separators, and blue only for links, focus, or meaningful information. Communicate hierarchy through typography and spacing before borders or containers. Reserve semantic colors for actual status and warnings.

Keep glass optional and confined to navigation, with a nearly opaque fallback. Reading surfaces stay solid. Prefer tonal changes to gradients, halos, colored glows, heavy shadows, or decorative illustrations. Keep interactions stable, without lift, scale, or bounce effects.

## Workflow

1. Read the target manual and its corresponding source document in `docs/`.
2. Establish the requested scope, identify beginner prerequisites, and distinguish implemented features from planned ones.
3. Apply this guide through `manuals/manuals.css`; reuse neighboring manual patterns. Use `manuals/manuals.js` only for progressive enhancement.
4. Preserve technical meaning, source citations, anchors, and verified command/configuration examples. Visual polish must never imply implementation.
5. Run the applicable completion checks below and report any checks that could not run.

## Layout

- Use a left navigation sidebar and an unframed content column.
- Keep the reading column at a maximum width of **820px**, excluding outer padding.
- Contain wide tables, code, and diagrams in locally scrollable regions; they must not widen the page.
- Use the spacing scale **4, 8, 16, 24, 32, 48, 64px**.
- Keep the first screen document-focused: KAM, manual title, scope, and implementation status. Use a restrained header, not a landing-page hero.
- Collapse navigation below 768px. Native disclosure controls must remain usable without JavaScript.
- Use `aria-current="page"` for the active manual, visible focus states, and predictable keyboard navigation.
- Keep page sections unframed. Use containers only for code, callouts, or genuinely distinct tools.

## Design Tokens

`manuals/manuals.css` owns the shared token values. Preserve compatibility aliases when changing tokens.

```css
:root {
  color-scheme: dark;
  --bg-0: #101012;
  --bg-1: #18181B;
  --bg-2: #222225;
  --bg-3: #2C2C30;
  --text-primary: #F5F5F7;
  --text-secondary: #C5C5CA;
  --text-muted: #A1A1A8;
  --signal: #80B8FF;
  --signal-hover: #AFD2FF;
  --signal-active: #599EF5;
  --success: #3DDC91;
  --warning: #F5A524;
  --danger: #FF5C7A;
  --info: #56C7FF;
  --glass-bg: rgba(24, 24, 27, .92);
  --glass-border: rgba(255, 255, 255, .08);
  --glass-highlight: rgba(255, 255, 255, .05);
  --border-subtle: rgba(255, 255, 255, .06);
  --border-default: rgba(255, 255, 255, .12);
  --kam-font-heading: -apple-system, BlinkMacSystemFont, "Helvetica Neue", "Segoe UI", sans-serif;
  --kam-font-body: -apple-system, BlinkMacSystemFont, "Helvetica Neue", "Segoe UI", sans-serif;
  --kam-font-code: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
  --kam-content-width: 820px;
  --kam-space-1: 4px;
  --kam-space-2: 8px;
  --kam-space-3: 16px;
  --kam-space-4: 24px;
  --kam-space-5: 32px;
  --kam-space-6: 48px;
  --kam-space-7: 64px;
  --radius-sm: 8px;
  --radius-md: 12px;
  --radius-lg: 16px;
  --shadow-panel: 0 1px 0 rgba(255, 255, 255, .025);
}
```

## Typography

Use native platform typography, including San Francisco on Apple systems via `-apple-system`. Use native monospace for commands, configuration, logs, schemas, and payloads. Do not download or bundle Apple's proprietary fonts; no external font service is required.

| Role | Desktop | Below 768px | Weight |
| --- | --- | --- | --- |
| H1 | 48px | 32px | 600 |
| H2 | 32px | 28px | 600 |
| H3 | 28px | 24px | 600 |
| H4 | 22px | 22px | 600 |
| H5 | 18px | 18px | 600 |
| Body | 16px | 16px | 400 |
| Small / navigation | 14px | 14px | 400-600 |
| Code | 14px | 14px | 400 |

At 360px and below, H1 may reduce to 30px. Keep font sizes fixed at breakpoints, not viewport-scaled. Use one H1 and a semantic heading hierarchy. Body line-height is 1.65; heading line-height is 1.2-1.3. Keep letter spacing at zero. Use soft white, rather than blue, for emphasized prose.

## Surfaces And Navigation

Use the page background for prose and `--bg-1` for secondary surfaces. Separate navigation from content with one subtle divider. A 16px navigation blur is optional; layout and readability must be unchanged when `backdrop-filter` is unsupported.

Active navigation uses a neutral tonal fill, a visible edge, stronger text, and `aria-current`. Hover uses a subtle tonal change. Focus uses the signal color with a clearly visible outline. Keep controls in place during interactions.

Use 8px radii for code and callouts. Larger radii are reserved for genuinely distinct tools or dialogs, not page sections. Shadows should be nearly imperceptible.

## Content And Voice

For major user guides, use the applicable sections in this order:

1. Overview
2. Concepts
3. Requirements
4. Installation
5. Configuration
6. Usage
7. Examples
8. Troubleshooting
9. Reference

Write concise, professional, active prose: "Configure the radio baud rate." or "KAM stores announcements in SQLite." Describe actual behavior only. Keep requirements, future plans, and current capabilities clearly distinguished with explicit text labels.

## Code And Tables

- Every code block must include a language identifier.
- Examples must be copy-paste ready and verified against actual implementations. Preserve whitespace and separate commands from expected output.
- Use solid `--bg-1`, a neutral `--border-default` border, 8px radius, and 16px padding for code blocks. Allow local horizontal scrolling.
- Use tables for commands, configuration, protocol fields, and schemas. Keep explanatory prose outside tables.
- Use strong table headers, soft row dividers, and 8-16px cell padding. Preserve keyboard access to horizontal scrolling.

## Callouts And Status

Use neutral secondary surfaces with a thin semantic edge and an explicit label: **Information**, **Tip**, **Warning**, or **Danger**. Keep semantic color on the edge and label rather than flooding the surface. Status badges must name the state, such as **Implemented**, **Planned**, or **Hardware verification pending**.

Meaning must never rely on color alone. Where icons help, use thin-outline Lucide or Heroicons, with accessible names for controls and decorative icons hidden from assistive technology.

## Diagrams And Screenshots

Prefer architecture, sequence, message-flow, and state diagrams in Mermaid, PlantUML, or SVG. Use charcoal nodes, soft white labels, thin neutral connections, and signal blue only for meaningful emphasis. Match diagram colors and fonts to the shared stylesheet.

Keep Mermaid diagrams outside initially collapsed containers. Essential information must remain available as prose, tables, or readable diagram source when external rendering fails. Give images descriptive alt text and useful captions. Crop screenshots to relevant content.

## Completion Checks

For every manual change, verify the applicable checks:

- **Content:** technical meaning and examples preserved; semantic headings, descriptive links, code language identifiers, and implementation-status labels present.
- **Audience:** a beginner can identify the purpose, prerequisites, next action, and expected outcome; unfamiliar terms are explained on first use and advanced details follow the introduction.
- **Accessibility:** WCAG AA contrast for text on actual surfaces, visible focus, keyboard navigation, accessible names, and proper landmarks. Transparency and blur must never obscure text or carry meaning.
- **Responsive:** desktop, mobile, 320px width, and 200% zoom; no clipped content, overlapping text, or page-wide horizontal scrolling.
- **Navigation:** anchors, cross-document links, disclosure controls, active states, and copy controls where present.
- **Degradation:** usable content with JavaScript disabled, native font fallbacks, unsupported glass effects, and unavailable external assets.
- **Motion:** respect `prefers-reduced-motion`.
- **Diagrams:** correct rendering, visible labels, and local scrolling on narrow screens.
- **Visual evidence:** capture desktop and mobile screenshots for layout changes.

A manual update is complete when technical meaning remains intact, applicable checks pass, and cross-references work. Report unverified checks explicitly. Updating this guide alone does not require automatic restyling of existing manuals.