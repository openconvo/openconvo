# MCP search

OpenConvo can make preserved community knowledge searchable from an MCP
client through one read-only tool. It supports two transports:

- local stdio, where the client starts `openconvo mcp`
- opt-in Streamable HTTP at `/mcp` on a running OpenConvo server

Both transports expose the same `search_messages` tool. Neither bypasses
deleted-message rules, exposes raw SQL, or adds an archive write path. Their
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

This grants that MCP client access to private archived message excerpts. Treat
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
credential and read archive excerpts. The proxy must preserve the
`Authorization` header and allow requests to `/mcp`.

Add the endpoint to Claude Code for the current user:

```bash
claude mcp add --transport http --scope user \
  --header "Authorization: Bearer $OPENCONVO_MCP_TOKEN" \
  openconvo https://archive.example.com/mcp
```

Alternatively, a project `.mcp.json` can reference an environment variable so
the secret is not committed:

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

The server advertises exactly one tool. It returns only live, non-deleted
messages and supports the same filters as the Search page.

| Argument | Meaning |
| --- | --- |
| `query` | Required search text, up to 500 characters |
| `mode` | `fts` (default, entirely local) or `semantic` |
| `channel_id` | OpenConvo channel UUID; results include it for follow-up calls |
| `author` | Case-insensitive username or display-name substring |
| `after` | Inclusive `YYYY-MM-DD` or RFC3339 lower bound |
| `before` | Exclusive `YYYY-MM-DD` or RFC3339 upper bound |
| `has_attachment` | `true` for messages with attachments, `false` for messages without |
| `limit` | Page size from 1–100; default 25 |
| `offset` | Page offset from 0–100000; use `next_offset` when `has_more` is true |

FTS uses OpenConvo's PostgreSQL `websearch_to_tsquery` search, including
quoted phrases and exclusions. Keyword excerpts retain the Search page's
`<mark>` highlighting delimiters as inert text.

Semantic mode has the same explicit privacy boundary as the Search page:
during a search, it sends only the query to the configured OpenAI embeddings
endpoint, then compares the returned vector against the local, disposable
pgvector index. It does not send matching archived messages during the query.

Indexing is a separate transfer: enabling optional message embeddings sends
each eligible archived message to OpenAI once to build that local index. See
[optional message embeddings](self-hosting.md#optional-message-embeddings)
before enabling it. Semantic mode reports clearly when embeddings are disabled,
not configured or still building; it never falls back silently to FTS.

Results include the message and channel IDs, channel/community names, minimal
author information, timestamp, bounded excerpt, and attachment presence. Actor
UUIDs, avatar URLs, source payloads, attachment URLs, and blob keys are omitted.
