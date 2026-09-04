# internal/viewer — Agent Guide

> For project-wide conventions (error sentinels, mock generation, build commands, interface naming,
> logging), see `.github/copilot-instructions.md`. This file covers only invariants local to the
> `viewer` package.

## Package map

| File | Responsibility |
|---|---|
| `viewer.go` | `Viewer` struct, `New()`, HTTP server setup, route registration, channel classification |
| `handlers.go` | All HTTP handlers, `mainView` data struct, `setConversation` helper |
| `filestorage.go` | `fileByIDStorage` optional extension interface + `fileByID` helper |
| `paging.go` | `messagePager` extension interface, page arithmetic, `pageFromSeq` fallback, `channelPage`, `messagePageOf` |
| `template.go` | Template compilation (`initTemplates`), FuncMap, sender classification |
| `templates/index.html` | All HTML template defines (full page + HTMX partials + JS) |
| `templates/styles.html` | All CSS (single `hx_css` define, CSS variables, dark mode) |

---

## Invariants

### 1. HTMX dual-mode rendering

Every handler that can be reached by both a direct browser URL and an HTMX partial swap **must**
branch on `isHXRequest(r)`:

- **HTMX request** → render the relevant partial template (e.g. `hx_conversation`, `hx_canvas`).
- **Direct / deep-link request** → render full `index.html` so the sidebar and layout are present.

Failing to do this breaks deep-link bookmarkability.

### 2. `setConversation` must be called in every channel-rendering handler

`setConversation(page, ci)` is the **only** place that sets both `page.Conversation` and
`page.CanvasAvailable`. It must be called in every handler that renders a view containing a channel
header, including deep-link fallback paths.

Current call sites: `channelHandler`, `threadHandler`, `canvasHandler`.  
If you add a new handler that renders a channel, add a `setConversation` call.

### 3. Optional extension interface — never extend `source.Storage` directly

When internal viewer code needs a capability that not all storage backends provide, use the pattern
in `filestorage.go`:

1. Declare an **unexported** interface in the viewer package (`fileByIDStorage`).
2. Use a runtime type assertion to check if the concrete storage implements it.
3. Degrade gracefully (return `fs.ErrNotExist`-wrapped error) if it does not.

Do **not** add the method to the public `source.Storage` interface — that is a breaking change for
third-party implementations.

### 4. Renderer must be assigned before `initTemplates`

`v.r` (the `renderer.Renderer`) must be fully initialised before `initTemplates(v)` is called in
`New()`, because the `rendertext` and `render` template functions close over `v.r`.  
Reordering these two steps causes a nil-dereference at first template execution.

### 5. Path component safety

All path components extracted from the URL (file IDs, timestamps, filenames) must be validated with
`isInvalid(pcomp)` before use in file system operations. `isInvalid` rejects `..`, `~`, `/`, `\`.

### 6. Canvas — graceful degradation, never error

Canvas support degrades silently when the storage backend does not implement `fileByIDStorage`.
The tab is shown **disabled** (HTML `disabled` attribute, CSS `.disabled`), never hidden or
erroring. `canvasContentHandler` logs at `DEBUG` level before returning 404 when the file is
absent or the storage does not support `FileByID`.

### 7. Canvas tab visibility

The `tab_list` partial (and therefore the tab bar) is only rendered when
`canvas_present .Conversation` returns true, i.e. `Channel.Properties.Canvas.FileId != ""`.
`Properties` is a pointer — it can be nil; `canvas_present` must guard against that (current
implementation does via the template func in `template.go`).

### 8. Template iframe sandbox

The canvas iframe uses `sandbox="allow-same-origin"` only — no scripts from canvas content may
execute. Do not add `allow-scripts` to this attribute.

### 9. Tab CSS — `box-shadow` not `border-bottom`

The active-tab indicator uses `box-shadow: inset 0 -3px 0 var(--primary-color)`.  
The inactive tab pre-reserves space with `box-shadow: inset 0 -3px 0 transparent`.  
This avoids layout shift on selection. Do not replace with `border-bottom`.

### 10. CSS — always use variables, never hardcoded colours

All colours must reference the CSS custom properties defined in `:root` (e.g. `--primary-color`,
`--bg-color`). Dark mode is driven purely by `@media (prefers-color-scheme: dark)` overriding
those variables. Hardcoded colour values break dark mode.

### 11. Sidebar `.channel-list` class is load-bearing

`channel_list` renders each sidebar group as `<details class="channel-group">` whose links are
wrapped in an inner `<div class="channel-list">`. `viewer.js` selects
`.channel-sidebar .channel-list a` in both `syncActiveChannel` and `onDocumentClick`, and the link
styling in `styles.html` is scoped to `.channel-list a`. Renaming or removing that inner div
silently breaks active-channel highlighting and all sidebar link styling.

### 12. Sidebar collapse must never depend on JavaScript

Groups collapse via native `<details>`/`<summary>`, and the open group is chosen server-side by
`channels.groups`. Static HTML export renders with `.Interactive` false and its pages contain no
`<script>` tags, so any JS-driven toggle would leave static output permanently expanded. `viewer.js`
may only *enhance* the behaviour, never own it.

The one enhancement it provides — `expandGroup` — must stay gated on `location.pathname` having
changed. `syncActiveChannel` is registered on **every** `htmx:afterSettle`, including thread panel,
user profile, alias form and tab swaps; expanding unconditionally there undoes a deliberate manual
collapse on every unrelated interaction. Note that the case this serves is browser back/forward
(htmx history restore), not sidebar clicks: a collapsed `<details>` does not expose its children, so
click-navigation always finds the target group already open.

`syncActiveChannel` also runs once from `init()` on first paint (`DOMContentLoaded`, or immediately
if the document has already loaded). On that call `group.open = true` is a no-op — the server
already rendered the active group open — but `expandGroup`'s other line,
`link.scrollIntoView({block: "nearest"})`, still fires, scrolling the active channel link into view.
That is what makes a deep link into a large group (hundreds of channels) land on the visible
sidebar entry instead of requiring a manual scroll.

### 13. Sidebar sorting depends on the source's user index

`initChannels` sorts each bucket by `st.UserIndex.ChannelName`. DM names resolve through
`ChannelName -> Username -> userattr`, so DM ordering depends on the user index being populated.

For `source.Dump` archives with no `users.json`, `Users` returns `source.ErrNotFound`, `New`
substitutes an empty index, and `userattr` falls back to `"<external>:<id>"` for every peer — so DM
entries sort by raw user ID rather than display name. This is not a regression: those names already
*displayed* that way. MPIM names come from the channel's `Purpose` field rather than the user index,
so group messages are unaffected. `source.Export` always returns an index; `source.ChunkDir`
propagates an unmapped error, so `New` fails outright rather than degrading.

### 14. Paging is opt-in and never applies to static output

`viewerOptions.pageSize` defaults to **0 (disabled)**, and `New` additionally forces it to 0
whenever `mode == renderer.ModeStatic`. `internal/convert/html.go` therefore gets whole-timeline
pages without asking for anything, and its output is unchanged by paging. Only `slackdump view`
opts in, via `-page-size` (default 100, `0` to disable).

A nil `mainView.Paging` means the timeline is unpaged: `paging_nav` renders nothing and the output
is byte-identical to the pre-paging viewer. Do not give `Paging` a non-nil zero value as a
"default" — the nil check *is* the switch.

### 15. `ChannelMessage` and `ChannelPageMessage` are not interchangeable

With paging on, `Routes.ChannelMessage` returns the `p`-permalink form
(`/archives/{id}/p1710063528879959`), because the page a message lives on is only known
server-side and resolving it per message would cost one query per rendered message. That URL is
served by `postRedirectHandler`.

`postRedirectHandler` must therefore redirect using `Routes.ChannelPageMessage`, which returns the
terminal `/archives/{id}?p=N#ts`. Using `ChannelMessage` there sends the handler back to itself —
an infinite redirect. This is not hypothetical: it shipped briefly during development and was
caught by a live `curl` returning `303` to the request URL. **Templates use `ChannelMessage`;
handlers use `ChannelPageMessage`.**

### 16. `pageFromSeq` walks the sequence exactly once

The `iter.Seq2` returned by `AllMessages` is backed by live `sql.Rows` for database sources;
iterating it a second time yields nothing. `pageFromSeq` keeps two buffers — the requested window
and a rolling window of the trailing page — because an out-of-range page always clamps to the
*last* page, so one pass covers every outcome. The rolling buffer is full-size, so it is trimmed to
`total - (pages-1)*size` before returning. Do not "simplify" this into a count pass followed by a
fetch pass.

### 17. The paging footer is a sibling of `.message-list`

`.message-list` is `flex: 1; overflow-y: auto`. `paging_nav` is rendered by
`hx_conversation_body` *after* `message_list`, not inside it; moving it inside makes the controls
scroll away with the messages. Its links carry the same HTMX attributes as the tab buttons, and
because `channelPartial` re-executes `hx_conversation` on every swap, each swap ships a freshly
computed footer — that is what makes clicking "Older" repeatedly work.

### 18. Non-database sources scan the timeline twice on a thread deep link

`messagePager` is implemented only by `internal/chunk/backend/dbase`. For export, dump and
chunkdir archives, `RenderThread` scans the channel once in `messagePageOf` and again in
`channelPage`'s `pageFromSeq` fallback.

This is a deliberate trade-off, not an oversight. Before paging, that path made one scan but
rendered the *entire* timeline (8.97 MB of HTML for a 9,550-message channel); it now makes two
scans and renders 100 messages, which is faster overall because rendering dominated. Collapsing
the two scans would mean threading an ordinal through `pageFromSeq`, and that complexity is not
worth it unless profiling on a large export says otherwise.

---

## Adding a new handler — checklist

1. Implement the handler as a method on `*Viewer` in `handlers.go`.
2. Check `isHXRequest(r)` and render the appropriate partial or full `index.html`.
3. Call `setConversation(page, ci)` if the handler renders any channel view.
4. Validate all URL-derived path components with `isInvalid` before file system use.
5. Register the route in `New()` in `viewer.go`.
6. Add any new template defines to `templates/index.html` (or a new `*.html` file in `templates/`).
7. Add any new template helper functions to the FuncMap in `initTemplates` (`template.go`).

---

## Template data contracts

### `render_message`

Receives a `messageView` (defined in `handlers.go`), **not** a bare `slack.Message`.  
Use the `msgview` template func to construct one at the call site:

```
{{ template "render_message" (msgview $channelID $msg) }}
```

| Field | Type | Purpose |
|---|---|---|
| `.Msg` | `slack.Message` | The message to render |
| `.ChannelID` | `string` | Channel ID for reply-to anchor link; pass `""` to suppress the reply banner |

Pass `""` as `$channelID` in the thread panel (`hx_thread`), where the parent message lives on a different page and the anchor link would be broken.

### `channel_list`

Ranges over `.Groups`, a `[]channelGroup` produced by `mainView.Groups` (`handlers.go`), which
delegates to `channels.groups(m.Conversation.ID)` (`viewer.go`).

| Field | Type | Purpose |
|---|---|---|
| `.Label` | `string` | Group heading, e.g. `Public channels` |
| `.ID` | `string` | DOM id suffix — `public`, `private`, `mpim`, `dm` |
| `.Items` | `[]slack.Channel` | Channels, pre-sorted alphabetically by `initChannels` |
| `.Open` | `bool` | Renders the `open` attribute on `<details>` |

Empty groups are omitted. Exactly one group is always `Open`, unless there are no channels at all.
Inside the nested `range` over `.Items`, use `$.Interactive` — `.` is a `slack.Channel` there.

`.Items` aliases the viewer's long-lived `channels` slices. Treat it as read-only: sorting or
appending in place would corrupt the shared state for every other request.

---

## Template ARIA contracts

The tablist/tabpanel pair must satisfy:

| Element | Required attributes |
|---|---|
| Tab list container | `role="tablist"` |
| Each tab button | `role="tab"`, `aria-selected`, `aria-controls="{panel-id}"`, `tabindex` (roving) |
| Conversation panel | `id="conversation-panel"`, `role="tabpanel"`, `aria-labelledby="tab-conversation"`, `tabindex="0"` |
| Canvas panel | `id="canvas-panel"`, `role="tabpanel"`, `aria-labelledby="tab-canvas"`, `tabindex="0"` |

Keyboard navigation (Arrow Left/Right, Home, End) is implemented as inline JS within the
`tab_list` template define.
