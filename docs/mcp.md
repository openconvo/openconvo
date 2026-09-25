# MCP search

OpenConvo can make preserved community knowledge searchable from an MCP
client through read-only tools: `search_messages` finds messages,
`get_message_context` reads one in its conversation, and `list_channels`
names the archived channels. It supports two transports:

- local stdio, where the client starts `openconvo mcp`
- opt-in Streamable HTTP at `/mcp` on a running OpenConvo server

Both transports expose the same tools. Neither bypasses deleted-message
rules, exposes raw SQL, or adds an archive write path. Their
PostgreSQL connections have `default_transaction_read_only=on`.

## Local stdio

```bash
openconvo mcp
```

The command speaks MCP over standard input/output and does not open a port.

### Connect a client

An MCP client configuration for a bare-process installation looks like this:

```json
{
  "mcpServers": {
    "openconvo": {
      "command": "/absolute/path/to/openconvo",
      "args": ["mcp"],
      "env": {
        "DATABASE_URL": "postgres://openconvo:password@localhost:5432/openconvo?sslmode=disable"
      }
    }
  }
}
```

For the standard Docker Compose installation, let the client start the command
inside the already-running application container. Use absolute paths because
desktop clients often start commands with an unrelated working directory:

```json
{
  "mcpServers": {
    "openconvo": {
      "command": "docker",
      "args": [
        "compose",
        "--project-directory", "/absolute/path/to/openconvo",
        "-f", "/absolute/path/to/openconvo/compose.yaml",
        "exec", "-T", "openconvo", "openconvo", "mcp"
      ]
    }
  }
}
```

`-T` is required because MCP owns stdin/stdout and must not be wrapped in a
pseudo-terminal. Client configuration formats differ, but the command and
arguments are the same.

This grants that MCP client access to private archived messages. Treat
the client configuration and the machine account that can launch it as
administrator access. Stdio needs no MCP token because the ability to launch
the local process and connect to the database is its security boundary.

## Remote Streamable HTTP

Remote MCP is disabled by default. It uses the existing OpenConvo HTTP
listener, so it does not add another port or service. Enable it on the server
with a dedicated random token:

```bash
openssl rand -hex 32
```

Add the result to the server's `.env`, then recreate the application container:

```dotenv
OPENCONVO_MCP_HTTP_ENABLED=true
OPENCONVO_MCP_TOKEN=replace-with-the-generated-token
```

```bash
docker compose up -d
```

The public MCP URL is the normal OpenConvo origin plus `/mcp`, for example
`https://archive.example.com/mcp`. Put OpenConvo behind a TLS-terminating
reverse proxy and use an HTTPS hostname. Do not send the bearer token to a
plain-HTTP IP address: anyone able to observe that connection can reuse the
credential and read archived messages. The proxy must preserve the
`Authorization` header and allow requests to `/mcp`.

Add the endpoint to Claude Code for the current user:

```bash
claude mcp add --transport http --scope user \
  --header "Authorization: Bearer $OPENCONVO_MCP_TOKEN" \
  openconvo https://archive.example.com/mcp
```

Alternatively, a project `.mcp.json` can reference an environment variable so
the secret is not committed. OpenConvo's repository ships this file as
`.mcp.json.example` and ignores `.mcp.json`:

```json
{
  "mcpServers": {
    "openconvo": {
      "type": "http",
      "url": "https://archive.example.com/mcp",
      "headers": {
        "Authorization": "Bearer ${OPENCONVO_MCP_TOKEN}"
      }
    }
  }
}
```

Set `OPENCONVO_MCP_TOKEN` in the environment that launches Claude Code, then
check the connection with `claude mcp list` or `/mcp` inside Claude Code. A
disabled endpoint returns `404`; a missing or incorrect token returns `401`.

The browser administrator password and login cookie deliberately do not grant
MCP access. The bearer token is a separate machine credential and should be
stored like an administrator secret. To rotate it, replace
`OPENCONVO_MCP_TOKEN`, recreate the OpenConvo container, and update clients.
This first remote version is intended for a single operator; it does not
implement multi-user identities or OAuth.

Remote MCP uses a small, dedicated read-only database pool. It does not run
migrations, ingestion, background jobs, or embedding generation. Requests are
stateless JSON, cross-origin requests are rejected, and request bodies are
bounded. TLS remains the responsibility of the deployment's reverse proxy.

## `search_messages`

It returns only live, non-deleted messages and supports the same filters as
the Search page.

| Argument | Meaning |
| --- | --- |
| `query` | Required search text, up to 500 characters |
| `mode` | `fts` (default, entirely local) or `semantic` |
| `channel_id` | OpenConvo channel UUID from `list_channels` or a result |
| `author` | Case-insensitive username or display-name substring |
| `after` | Inclusive lower bound: `YYYY-MM-DD` (midnight UTC) or RFC3339 |
| `before` | Exclusive upper bound: `YYYY-MM-DD` (midnight UTC) or RFC3339 |
| `has_attachment` | `true` for messages with attachments, `false` for messages without |
| `limit` | Page size from 1–100; default 25 |
| `offset` | Page offset from 0–100000; use `next_offset` when `has_more` is true |

A date without a time is midnight UTC. To cover a local day, pass RFC3339
timestamps with the offset, such as `2026-09-01T00:00:00+02:00`.

FTS uses OpenConvo's PostgreSQL `websearch_to_tsquery` search, including
quoted phrases, `or`, and `-` exclusions. It matches whole words, ignoring
letter case and accents: `cafe` finds `café`, but `bake` does not find
`baked`, so list the forms you need with `or`. There are no wildcards, emoji
and punctuation are not indexed, and a web address matches only as a whole
host, so search `www.example.com` rather than `example.com`.
A query with no searchable words at all, such as an emoji alone, returns an
error rather than an empty page, and so does a `channel_id` that names no
archived channel.

Each result carries the message itself: its text in `content`, cut at 2000
characters with `…` and `content_truncated: true`, and when present
`edited_at`, `kind` (for anything but an ordinary message, such as `reply`,
`pin` or `member_join`), `reply_to_message_id`, `stickers`, `attachments`
(filename, content type, size and description) and `reactions` (emoji and
count). In FTS mode it also carries `excerpt`, the matching passage with the
Search page's `<mark>` highlighting delimiters as inert text. A message up to
500 characters comes back whole there; a longer one is cut to a passage around
the match, starting or ending with `…` where it leaves text out.

Semantic mode has the same explicit privacy boundary as the Search page:
during a search, it sends only the query to the configured OpenAI embeddings
endpoint, then compares the returned vector against the local, disposable
pgvector index. It does not send matching archived messages during the query.

Indexing is a separate transfer: enabling optional message embeddings sends
each eligible archived message to OpenAI once to build that local index. See
[optional message embeddings](self-hosting.md#optional-message-embeddings)
before enabling it. Semantic mode reports clearly when embeddings are disabled,
not configured or still building; it never falls back silently to FTS.

Results also include the channel ID and channel/community names, minimal
author information, timestamp, and attachment presence. Semantic results carry
`distance`, the cosine distance between the query and the message: lower is
closer. Nearest-neighbour search always returns something, so compare
distances rather than trusting the first result. Actor UUIDs, avatar URLs,
source payloads, attachment URLs, blob keys, and bookmarks are omitted.

## `get_message_context`

Returns one live message with the conversation around it, the same window the
web UI's `/messages/:id` page shows.

| Argument | Meaning |
| --- | --- |
| `message_id` | Required message UUID from a search result or an earlier context |
| `before` | Earlier messages from the same channel, 0–25; default 5 |
| `after` | Later messages from the same channel, 0–25; default 5 |

Messages come oldest first, each in the shape of a search result's message
with its full, uncut `content`, together with the `channel` (ID, name, kind,
topic, parent, community, message count and latest message time) and the
`target_message_id`. `more_before` and `more_after` say whether the channel
continues past the window; to read further, call the tool again with the first
or last message. A thread is a channel of its own. A deleted or never-archived
message answers "message not found", and deleted messages never appear in the
window.

## `list_channels`

Takes no arguments and lists the channels the other tools can read, in the
archive browser's order: each channel's `channel_id`, `name`, `kind`, `topic`,
`parent_name`, `community_name`, `message_count` (live messages) and
`last_message_at`. Categories are left out because they hold no messages;
their names appear as the parent of the channels under them. A thread is a
channel of its own, so a search filtered to a parent channel leaves out its
threads. A channel missing from the list is not archived, which tells "never
discussed" apart from "not archived".
