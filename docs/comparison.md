# How OpenConvo compares

Several tools sit between a chat platform and the knowledge inside it. They
are not all doing the same job, and picking the wrong one is an expensive
mistake to discover years later. This page is meant to help you decide
quickly, including deciding against OpenConvo.

## Two different jobs

**Publishing** takes conversations that are already public and puts them
somewhere the world can find them: web pages, search engines, AI assistants.
Success is measured in discovery. The content is public by definition, so the
questions that matter are indexing, ranking and presentation.

**Custody** keeps a faithful, private copy of conversations under your own
control, so they survive deletion, platform changes and time. Success is
measured in fidelity and durability. The content may be sensitive, so the
questions that matter are consent, deletion, integrity and portability.

OpenConvo does custody. It has no public pages at all, by design: the archive
sits behind an administrator login, and
[public archive publishing is a deliberate non-goal](roadmap.md) for 1.0.

## Choose OpenConvo if

- Some of the conversations worth keeping are **not public**: a paid
  community, a customer support server, an internal or moderator channel.
- You need the **whole conversation**, not just answered questions: edits,
  deletions, reactions, threads, attachments, and the discussions that never
  had a question in them.
- You want the data **in your own infrastructure**, in formats that outlive
  the vendor: your PostgreSQL, your own disk or bucket, and
  [checksummed exports](archive-format.md) you can verify offline.
- You have to **answer for the data**: honor deletion requests, show what was
  removed and when, keep off-site backups, host in a particular jurisdiction.
- You want **your own AI tools reading a copy you control**. The read-only
  [MCP endpoint](mcp.md) serves your archive, so an assistant answers from
  what you actually kept, not from a vendor's index of what it chose to
  publish.

## Choose something else if

- Your goal is for **strangers to find your answers on Google** or in an AI
  assistant. OpenConvo does not publish anything. Reach for a tool built for
  discovery. For Discord, [Answer Overflow](https://www.answeroverflow.com)
  is the established one: it is open source, indexes public channels into
  search engines, and offers a hosted service, so there is nothing to run.
- You want to **replace chat itself** with a forum. That is a different
  decision about where your community lives; OpenConvo does not move it, it
  keeps a copy of it.
- You need a **one-off dump** rather than a running archive. A single-run
  exporter is simpler than a service, if you accept that the copy stops being
  true the moment it finishes.

## Running both is reasonable

Publishing and custody do not conflict. A company can index its public
support forum for discovery *and* keep a complete private archive of every
channel it is responsible for. The two tools read the same Discord server
through separate bots and never touch each other's data.

If you do run both, the useful mental model is: the published site is the
storefront, the archive is the vault. One is optimized for being found; the
other is optimized for never being lost.

## What OpenConvo will not become

So that the tradeoffs stay predictable, some things are permanently out of
scope rather than merely unbuilt: OpenConvo will not become a chat platform,
and the open-source edition will not be crippled to sell a paid tier;
capture, browse, search, curate and export stay complete. The
[roadmap](roadmap.md) lists the rest of the non-goals, and
[CONTRIBUTING.md](../CONTRIBUTING.md) explains the licensing behind them.
