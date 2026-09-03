import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { fetchSyncOverview, fetchSystemStatus, type SyncRow, type SystemStatus } from "../api";
import SyncChip from "../components/SyncChip";
import UpdatePanel from "../components/UpdatePanel";
import { errorMessage } from "./errorMessage";

type LoadState =
  | { phase: "loading" }
  | { phase: "error"; message: string }
  | { phase: "ready"; status: SystemStatus };

const numberFormat = new Intl.NumberFormat();

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(1)} ${units[unit]}`;
}

export default function Dashboard() {
  const [state, setState] = useState<LoadState>({ phase: "loading" });
  const [syncRows, setSyncRows] = useState<SyncRow[]>([]);

  useEffect(() => {
    let cancelled = false;
    const load = () => {
      fetchSystemStatus()
        .then((status) => {
          if (!cancelled) setState({ phase: "ready", status });
        })
        .catch((err: unknown) => {
          if (!cancelled) {
            setState({
              phase: "error",
              message: errorMessage(err, "Check that the OpenConvo server is running, then reload."),
            });
          }
        });
      fetchSyncOverview()
        .then((rows) => {
          if (!cancelled) setSyncRows(rows);
        })
        .catch(() => {});
    };
    load();
    const timer = setInterval(load, 15000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, []);

  return (
    <div className="page">
      <header className="page-header">
        <h1>Dashboard</h1>
        {state.phase === "ready" && (
          <span className="version-chip">
            v{state.status.version.version}
          </span>
        )}
      </header>

      {state.phase === "loading" && <div className="card muted">Loading system status…</div>}

      {state.phase === "error" && (
        <div className="card error-card" role="alert">
          <strong>Cannot reach the OpenConvo API.</strong>
          <p className="muted">{state.message}</p>
        </div>
      )}

      {state.phase === "ready" && <StatusGrid status={state.status} syncRows={syncRows} />}
    </div>
  );
}

function StatusGrid({ status, syncRows }: { status: SystemStatus; syncRows: SyncRow[] }) {
  const counts = status.counts;
  return (
    <>
      {status.insecure_public_access && (
        <div className="card warning-card">
          <strong>This archive is being served over plain HTTP</strong>
          <p>
            A request reached OpenConvo from outside this network without HTTPS anywhere in
            its path. Archived conversations, the administrator password and the session
            cookie all cross the network in the clear.
          </p>
          <p className="muted">
            Terminate TLS in front of OpenConvo and publish its port on{" "}
            <code>127.0.0.1</code> only — the{" "}
            <a
              href="https://github.com/openconvo/openconvo/blob/main/docs/self-hosting.md#exposing-it-to-your-community"
              target="_blank"
              rel="noreferrer"
            >
              self-hosting guide
            </a>{" "}
            has a three-line Caddyfile for it.
          </p>
        </div>
      )}

      {!status.discord.configured && (
        <div className="card notice-card">
          <strong>Get started: connect Discord</strong>
          <ol>
            <li>
              Create a bot application in the{" "}
              <a href="https://discord.com/developers/applications" target="_blank" rel="noreferrer">
                Discord developer portal
              </a>{" "}
              and enable the <em>Message Content</em> intent.
            </li>
            <li>
              Set <code>DISCORD_TOKEN</code> and <code>DISCORD_APPLICATION_ID</code> in your{" "}
              <code>.env</code> file — the next step needs the application ID for the invite link.
            </li>
            <li>
              Restart OpenConvo, then finish setup on the <Link to="/discord">Discord page</Link>.
            </li>
          </ol>
          <p className="muted">
            Nothing is archived until you explicitly select channels — see the{" "}
            <a
              href="https://github.com/openconvo/openconvo/blob/main/docs/self-hosting.md"
              target="_blank"
              rel="noreferrer"
            >
              self-hosting guide
            </a>
            .
          </p>
        </div>
      )}

      <UpdatePanel />

      <div className="stat-grid">
        <StatCard label="Communities" value={counts ? numberFormat.format(counts.communities) : "—"} />
        <StatCard label="Channels" value={counts ? numberFormat.format(counts.channels) : "—"} />
        <StatCard label="Messages" value={counts ? numberFormat.format(counts.messages) : "—"} />
        <StatCard label="Attachments" value={counts ? numberFormat.format(counts.attachments) : "—"} />
      </div>

      {status.attachments && (
        <div className="card">
          <h2>Attachment storage</h2>
          <p className="muted">
            {formatBytes(status.attachments.stored_bytes)} stored ·{" "}
            {numberFormat.format(status.attachments.stored)}{" "}
            {status.attachments.stored === 1 ? "attachment" : "attachments"}
            {status.attachments.pending > 0 &&
              ` · ${numberFormat.format(status.attachments.pending)} pending`}
            {status.attachments.failed > 0 &&
              ` · ${numberFormat.format(status.attachments.failed)} failed`}
          </p>
          {!status.attachments.enabled && status.attachments.pending > 0 && (
            <p className="muted">
              Those pending attachments are archived by reference only. Set{" "}
              <code>OPENCONVO_ATTACHMENTS_ENABLED=true</code> to store them in OpenConvo.
            </p>
          )}
        </div>
      )}

      {syncRows.length > 0 && (
        <div className="card">
          <h2>Channels being archived</h2>
          <ul className="channel-list">
            {syncRows.map((row) => (
              <li key={row.channel_id} className="channel-row">
                <span className="channel-name">
                  <Link to={`/channels/${row.channel_id}`}>#{row.channel_name}</Link>{" "}
                  <span className="muted">· {row.community_name}</span>
                </span>
                <SyncChip row={row} />
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="card-grid">
        <div className="card">
          <h2>Discord</h2>
          <Pill
            ok={status.discord.connected}
            okText="Connected"
            badText={status.discord.configured ? "Disconnected" : "Not configured"}
            neutralBad={!status.discord.configured}
          />
          <p className="muted">
            {status.discord.connected
              ? (status.discord.bot_username
                  ? "@" + status.discord.bot_username
                  : "The bot") + " is receiving events."
              : status.discord.last_error ??
                (status.discord.configured
                  ? "A bot token is configured, but the Gateway is not connected."
                  : "Nothing is archived until a bot token is configured.")}{" "}
            <Link to="/discord">Manage Discord</Link>
          </p>
        </div>
        <div className="card">
          <h2>Database</h2>
          <Pill ok={status.database.connected} okText="Connected" badText="Unreachable" />
          <p className="muted">
            {status.database.connected
              ? `PostgreSQL · schema v${status.database.schema_version ?? "?"}`
              : status.database.error ?? "The archive database cannot be reached."}
          </p>
        </div>
        <div className="card">
          <h2>Storage</h2>
          <Pill ok okText={status.storage.driver} badText="" />
          <p className="muted">{status.storage.path ?? "Attachment blob storage."}</p>
        </div>
        <div className="card">
          <h2>Build</h2>
          <p className="mono muted">
            {status.version.version} · {status.version.commit.slice(0, 10)}
            <br />
            {status.version.go_version}
            <br />
            up since {new Date(status.started_at).toLocaleString()}
          </p>
        </div>
      </div>
    </>
  );
}

function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="card stat-card">
      <div className="stat-value">{value}</div>
      <div className="stat-label">{label}</div>
    </div>
  );
}

function Pill({
  ok,
  okText,
  badText,
  neutralBad = false,
}: {
  ok: boolean;
  okText: string;
  badText: string;
  neutralBad?: boolean;
}) {
  const className = ok ? "pill pill-ok" : neutralBad ? "pill pill-neutral" : "pill pill-bad";
  return <span className={className}>{ok ? okText : badText}</span>;
}
