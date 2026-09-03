import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  fetchArchiveChannels,
  fetchChannelMessages,
  type ArchiveChannel,
  type ArchiveMessage,
} from "../api";
import MessageTimeline from "../components/MessageTimeline";
import { errorMessage } from "./errorMessage";

type TimelineState =
  | { phase: "loading" }
  | { phase: "error"; message: string }
  | {
      phase: "ready";
      channel: ArchiveChannel;
      messages: ArchiveMessage[];
      hasOlder: boolean;
    };

export default function ChannelTimeline() {
  const { channelId = "" } = useParams();
  const [state, setState] = useState<TimelineState>({ phase: "loading" });
  const [channels, setChannels] = useState<ArchiveChannel[]>([]);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState("");

  useEffect(() => {
    let cancelled = false;
    setState({ phase: "loading" });
    Promise.all([fetchChannelMessages(channelId), fetchArchiveChannels()])
      .then(([page, allChannels]) => {
        if (!cancelled) {
          setState({
            phase: "ready",
            channel: page.channel,
            messages: page.messages,
            hasOlder: page.has_older,
          });
          setChannels(allChannels);
        }
      })
      .catch((error: unknown) => {
        if (!cancelled) {
          setState({ phase: "error", message: errorMessage(error, "This channel could not be loaded.") });
        }
      });
    return () => { cancelled = true; };
  }, [channelId]);

  const loadOlder = async () => {
    if (state.phase !== "ready" || state.messages.length === 0 || loadingOlder) return;
    setLoadingOlder(true);
    setOlderError("");
    try {
      const page = await fetchChannelMessages(channelId, state.messages[0].id);
      setState((current) => current.phase === "ready" ? {
        ...current,
        messages: [...page.messages, ...current.messages],
        hasOlder: page.has_older,
      } : current);
    } catch (error) {
      // Keep the conversation on screen; only the extra page failed.
      setOlderError(errorMessage(error, "Could not load older messages. Try again."));
    } finally {
      setLoadingOlder(false);
    }
  };

  if (state.phase === "loading") return <div className="page"><div className="card muted">Loading conversation…</div></div>;
  if (state.phase === "error") return <div className="page"><div className="card error-card" role="alert">{state.message}</div></div>;

  const parent = state.channel.parent_kind !== "category"
    ? channels.find((channel) => channel.id === state.channel.parent_channel_id)
    : undefined;
  const threads = channels.filter((channel) => channel.parent_channel_id === state.channel.id);
  const currentlySyncing = state.channel.archive_enabled || Boolean(parent?.archive_enabled);

  return (
    <div className="page conversation-page">
      <nav className="breadcrumbs" aria-label="Breadcrumb">
        <Link to="/channels">Archive</Link>
        <span aria-hidden="true">/</span>
        {parent && <><Link to={`/channels/${parent.id}`}>#{parent.name}</Link><span aria-hidden="true">/</span></>}
        <span>{state.channel.kind.includes("thread") ? "↳" : "#"}{state.channel.name}</span>
      </nav>
      <header className="conversation-header">
        <div>
          <h1>{state.channel.kind.includes("thread") ? "↳" : "#"}{state.channel.name}</h1>
          {state.channel.topic && <p>{state.channel.topic}</p>}
          <p className="muted">{state.channel.message_count.toLocaleString()} archived messages</p>
          {!currentlySyncing && (
            <p className="muted">
              New messages are not being archived: this channel is not enabled on the{" "}
              <Link to="/discord">Discord page</Link>. Everything already archived is kept.
            </p>
          )}
        </div>
      </header>

      {threads.length > 0 && (
        <section className="thread-strip" aria-labelledby="threads-heading">
          <h2 id="threads-heading">Threads</h2>
          <div>
            {threads.map((thread) => (
              <Link key={thread.id} to={`/channels/${thread.id}`}>
                {thread.name} <span>{thread.message_count.toLocaleString()}</span>
              </Link>
            ))}
          </div>
        </section>
      )}

      {olderError && <div className="form-error" role="alert">{olderError}</div>}
      {state.hasOlder && (
        <div className="older-control">
          <button type="button" onClick={loadOlder} disabled={loadingOlder}>
            {loadingOlder ? "Loading…" : "Load older messages"}
          </button>
        </div>
      )}
      {state.messages.length === 0 ? (
        <div className="card muted">No messages have been archived in this channel yet.</div>
      ) : (
        <MessageTimeline messages={state.messages} />
      )}
    </div>
  );
}
