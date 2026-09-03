import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { fetchMessageContext, type MessageContext as Context } from "../api";
import MessageTimeline from "../components/MessageTimeline";
import { errorMessage } from "./errorMessage";

type LoadState =
  | { phase: "loading" }
  | { phase: "error"; message: string }
  | { phase: "ready"; context: Context };

export default function MessageContext() {
  const { messageId = "" } = useParams();
  const [state, setState] = useState<LoadState>({ phase: "loading" });

  useEffect(() => {
    let cancelled = false;
    setState({ phase: "loading" });
    fetchMessageContext(messageId)
      .then((context) => {
        if (!cancelled) setState({ phase: "ready", context });
      })
      .catch((error: unknown) => {
        if (!cancelled) {
          setState({ phase: "error", message: errorMessage(error, "This message could not be loaded.") });
        }
      });
    return () => { cancelled = true; };
  }, [messageId]);

  useEffect(() => {
    if (state.phase !== "ready") return;
    document.getElementById(`message-${state.context.target_id}`)?.scrollIntoView({ block: "center" });
  }, [state]);

  if (state.phase === "loading") return <div className="page"><div className="card muted">Loading message context…</div></div>;
  if (state.phase === "error") return <div className="page"><div className="card error-card" role="alert">{state.message}</div></div>;

  return (
    <div className="page conversation-page">
      <nav className="breadcrumbs" aria-label="Breadcrumb">
        <Link to="/channels">Archive</Link>
        <span aria-hidden="true">/</span>
        <Link to={`/channels/${state.context.channel.id}`}>#{state.context.channel.name}</Link>
        <span aria-hidden="true">/</span>
        <span>Message</span>
      </nav>
      <header className="conversation-header context-header">
        <div>
          <h1>Message in context</h1>
          <p className="muted">The twenty messages before and after this one.</p>
        </div>
        <Link className="secondary-button" to={`/channels/${state.context.channel.id}`}>Open channel</Link>
      </header>
      <MessageTimeline messages={state.context.messages} targetId={state.context.target_id} />
    </div>
  );
}
