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

### Words and Contains

Two ways of matching are offered, because neither wins outright:

`Words (FTS)`, the default, matches whole words using a full-text index. It is
the faster of the two and it ignores accents, so `resume` finds `résumé`. It
does not match inside words: `log` will not find `login`. A trailing `*` turns
a term into a prefix search, which is how you reach the words a stem should
have found — `nation*` matches `national` and `nationwide`.

`Contains` matches any substring, so `log` does find `login`. It is the slower
of the two and it returns far more noise on short terms, but it needs no index
and it finds fragments that word matching cannot. Accents are significant
here: a term written without them will not match the accented spelling.

Both modes are case-insensitive, accented characters included, and both treat
the term literally, so `%` and `_` have no special meaning.

Results are newest first. In `Words` mode you can sort by relevance instead;
`Contains` has no relevance to sort by, so the control is greyed out there. At
most 500 matches are shown and the panel says so when there were more.

### The index

`Words` mode needs a full-text index. The viewer builds it inside the archive
the first time you run a word search, which takes a moment on a large archive
and nothing thereafter. Nothing else pays for it: commands that never search
never build it.

Each run of the viewer checks whether the archive has changed since the index
was built and rebuilds it if so. The check happens once, at the first word
search of that run, so a viewer left open while an archive is being written to
keeps serving the index it started with: new messages appear in `Contains`
straight away, and in `Words` after a restart.

The index is an addition to the archive, not a change to its schema, so an
archive this viewer has indexed still opens in any other build of slackdump.

If the index cannot be built, the panel says so and `Contains` still works.
Note that an archive the viewer cannot write to at all will not open in the
first place, because opening one applies any pending schema migrations, so in
practice this state means a transient write failure such as another program
holding the archive open.

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
