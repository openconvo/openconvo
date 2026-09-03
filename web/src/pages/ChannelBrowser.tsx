import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { fetchArchiveChannels, type ArchiveChannel } from "../api";
import { errorMessage } from "./errorMessage";

type LoadState =
  | { phase: "loading" }
  | { phase: "error"; message: string }
  | { phase: "ready"; channels: ArchiveChannel[] };

const threadKinds = new Set(["thread", "private_thread"]);

function ChannelLink({ channel }: { channel: ArchiveChannel }) {
  return (
    <Link to={`/channels/${channel.id}`} className="archive-channel-link">
      <span className="archive-channel-main">
        <span className="channel-symbol">{threadKinds.has(channel.kind) ? "↳" : "#"}</span>
        <span>
          <strong>{channel.name || "unnamed-channel"}</strong>
          {channel.topic && <span className="channel-topic">{channel.topic}</span>}
        </span>
      </span>
      <span className="archive-channel-meta">
        {channel.is_private && <span className="pill pill-neutral">private</span>}
        <span>{channel.message_count.toLocaleString()} messages</span>
      </span>
    </Link>
  );
}

export default function ChannelBrowser() {
  const [state, setState] = useState<LoadState>({ phase: "loading" });

  useEffect(() => {
    let cancelled = false;
    fetchArchiveChannels()
      .then((channels) => {
        if (!cancelled) setState({ phase: "ready", channels });
      })
      .catch((error: unknown) => {
        if (!cancelled) {
          setState({ phase: "error", message: errorMessage(error, "The archive could not be loaded.") });
        }
      });
    return () => { cancelled = true; };
  }, []);

  const communities = useMemo(() => {
    if (state.phase !== "ready") return [];
    const grouped = new Map<string, ArchiveChannel[]>();
    for (const channel of state.channels) {
      const list = grouped.get(channel.community_name) ?? [];
      list.push(channel);
      grouped.set(channel.community_name, list);
    }
    return [...grouped.entries()];
  }, [state]);

  return (
    <div className="page archive-page">
      <header className="page-header archive-header">
        <div>
          <h1>Archive</h1>
          <p className="page-intro">Browse archived channels and threads.</p>
        </div>
      </header>

      {state.phase === "loading" && <div className="card muted">Loading archive…</div>}
      {state.phase === "error" && (
        <div className="card error-card" role="alert">{state.message}</div>
      )}
      {state.phase === "ready" && state.channels.length === 0 && (
        <div className="card notice-card">
          <strong>The archive is empty.</strong>
          <p className="muted">Select a channel under <Link to="/discord">Discord</Link> to start archiving it.</p>
        </div>
      )}
      {communities.map(([community, channels]) => {
        const roots = channels.filter((channel) => !threadKinds.has(channel.kind));
        const threadsByParent = new Map<string, ArchiveChannel[]>();
        for (const channel of channels.filter((candidate) => threadKinds.has(candidate.kind))) {
          if (!channel.parent_channel_id) continue;
          const threads = threadsByParent.get(channel.parent_channel_id) ?? [];
          threads.push(channel);
          threadsByParent.set(channel.parent_channel_id, threads);
        }
        let lastCategory = "";
        return (
          <section key={community} className="archive-community" aria-labelledby={`community-${channels[0]?.community_id}`}>
            <h2 id={`community-${channels[0]?.community_id}`}>{community}</h2>
            <div className="archive-channel-list">
              {roots.map((channel) => {
                const category = channel.parent_kind === "category" ? channel.parent_channel_name || "Other" : "";
                const showCategory = category !== lastCategory;
                lastCategory = category;
                const threads = threadsByParent.get(channel.id) ?? [];
                return (
                  <div key={channel.id} className="archive-channel-group">
                    {showCategory && category && <h3 className="category-label">{category}</h3>}
                    <ChannelLink channel={channel} />
                    {threads.length > 0 && (
                      <div className="archive-thread-list">
                        {threads.map((thread) => <ChannelLink key={thread.id} channel={thread} />)}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          </section>
        );
      })}
    </div>
  );
}
