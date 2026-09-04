# View Command

The `view` command allows you to view the contents of an archive, 
export, or dump directory or ZIP file.

It is a read-only command that does not modify the contents of the
specified directory or file.

Viewer supports displaying downloaded images, videos as well as remote
content.

The viewer uses a side panel for threads and user profiles, keeps the active
channel highlighted while navigating, and reports connection problems if the
local viewer server becomes unreachable.

Conversations are listed in four collapsible sidebar groups — public channels,
private channels, group messages and direct messages — each sorted
alphabetically and labelled with its conversation count. The group holding the
conversation you are viewing is expanded automatically.

Long conversations are shown 100 messages at a time, newest page first, with
"Older" and "Newer" links at the bottom of the message list. The page number is
part of the URL (`?p=12`), so any page can be bookmarked, and links to
individual messages resolve to the page holding them. Use `-page-size` to
change how many messages a page holds, or `-page-size 0` to render whole
conversations at once:

```bash
slackdump view -page-size 250 <directory_or_file>
```

## Search

Database archives can be searched from the viewer. Type in the search box at
the top of the sidebar and results appear in the right-hand panel as you type;
a scope selector switches between searching every conversation and searching
only the one you are viewing.

Clicking a result opens that conversation on the page holding the message and
highlights it, without closing the results list, so you can work through the
matches one at a time. `Prev`/`Next` at the foot of the panel step between
hits, as do the `n` and `N` keys. A result inside a thread opens the thread
itself, with a link back to the conversation.

Matching is case-insensitive, including accented characters, so a word typed
in any case is found. Accents themselves are not stripped: a term written
without them will not match the accented spelling. The search term is
matched literally, so characters such as `%` and `_` have no special meaning.
At most 500 matches are shown, newest first; the panel says so when there were
more.

Search requires a database archive, because it needs an indexed store to query.
Chunk, export and dump archives show no search box at all rather than a slow
one. Convert them first:

```bash
slackdump convert -f database -o <output_directory> <directory_or_file>
```

## Usage

```bash
slackdump view <directory_or_file>
```

If you experience problems viewing, run the viewer with DEBUG mode
enabled, and report the violating message to the GitHub Issues page.

```bash
DEBUG=1 slackdump view <directory_or_file>
```

It is recommended that you remove all sensitive information from the
JSON before sharing it, and also, to encrypt your message, you can use
the `slackdump tools encrypt` command, for example:

```bash
cat your_message.txt | slackdump tools encrypt > encrypted_message.txt
```

This will encrypt it using the embedded GPG public key, and can only be
encrypted by the author.
