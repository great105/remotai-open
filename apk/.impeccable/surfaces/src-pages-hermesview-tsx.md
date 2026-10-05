---
version: 1
slug: "src-pages-hermesview-tsx"
primary_target: "src/pages/HermesView.tsx"
related_targets: ["src/hermes/hermes.css","src/hermes/HermesChatSidebar.tsx","src/hermes/HermesAutomation.tsx","src/hermes/automation.css"]
---

Scope: Hermes chat and its settings only. Mode: Operate. Native app, web and Telegram share the React client. Novice users work equally often on phone and computer. Goal: choose a project folder on the selected host, describe a task, understand the response, return to the right conversation, and manage later work and persistent context.

## Direction contract

THESIS: Familiar conversation with an explicit project context; advanced controls stay reachable.

OWN-WORLD: Near-white #fafafa, dark text, neutral capsules, flat lists and quiet separators. Light palette follows the owner's screenshots; surrounding Remotai navigation remains.

STORY: Choose the computer, choose its folder, describe the task; return through history. Chat and Work share one run and draft. Settings group scheduled jobs, memory/context, and skills as plain-language disclosures. Jobs start separate conversations; a repeated job can reuse its own previous result. Preparing a memory or project instruction appends to the existing draft for the user to send.

FIRST VIEWPORT: History left, Chat/Work centered, new chat right; folder action in the empty conversation, suggestions above a bottom capsule composer. Wide screens expose history permanently.

FORM: User-pinned ChatGPT Android references supplied 01.10.2026, adapted directly in code. No concept roll or generated comp; screenshot structure is the authority. Voice and unsupported destinations are omitted; project context is explicit.

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance

## Quality bar

The supplied screenshots set the hierarchy and visual bar: spacious conversation, left drawer with search and real history, quiet segmented control and compact capsule input. Task targets remain at least 44px, input at least 16px, selected folder visible, and approval/stop controls reachable. Settings use real Hermes APIs, clearly state that scheduled work needs the computer and Hermes running, preserve drafts, and require explicit confirmation for deletion. Use real session and runtime data, with no fabricated agents, project counts or task results. No raster assets are required for this interface.
