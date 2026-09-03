import { useCallback, useEffect, useRef, useState } from "react";
import {
  fetchChannels,
  fetchCommunities,
  fetchSyncOverview,
  fetchSystemStatus,
  setChannelArchive,
  type Channel,
  type Community,
  type DiscordStatus,
  type SyncRow,
} from "../api";
import SyncChip from "../components/SyncChip";
import { errorMessage } from "./errorMessage";

// Kinds that hold messages or threads. Threads follow their parent
// channel and have kinds of their own, so kind alone decides this — a
// channel's parent is usually just the category it sits in.
//
// The API does not publish this list, and it rejects anything outside it, so
// this is a copy of selectableKinds in internal/http/handlers.go. Change both
// together.
const SELECTABLE = new Set(["text", "announcement", "forum", "media"]);

function inviteURL(applicationID: string): string {
  const params = new URLSearchParams({
    client_id: applicationID,
    scope: "bot applications.commands",
    permissions: "66560",
  });
  return "https://discord.com/oauth2/authorize?" + params.toString();
}

export default function Discord() {
  const [discord, setDiscord] = useState<DiscordStatus | null>(null);
  const [communities, setCommunities] = useState<Community[] | null>(null);
  const [selected, setSelected] = useState("");
  const [channels, setChannels] = useState<Channel[] | null>(null);
  const [sync, setSync] = useState<Map<string, SyncRow>>(new Map());
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [pendingDisable, setPendingDisable] = useState<Channel | null>(null);
  const keepArchivingRef = useRef<HTMLButtonElement>(null);

  const refreshOverview = useCallback(async () => {
    setRefreshing(true);
    setError("");
    const [statusResult, communitiesResult, syncResult] = await Promise.allSettled([
      fetchSystemStatus(),
      fetchCommunities(),
      fetchSyncOverview(),
    ]);

    if (statusResult.status === "fulfilled") {
      setDiscord(statusResult.value.discord);
    }
    if (communitiesResult.status === "fulfilled") {
      const communityList = communitiesResult.value;
      setCommunities(communityList);
      setSelected((current) =>
        communityList.some((community) => community.id === current)
          ? current
          : (communityList[0]?.id ?? ""),
      );
    }
    if (syncResult.status === "fulfilled") {
      setSync(new Map(syncResult.value.map((row) => [row.channel_id, row])));
    }

    const failed = [statusResult, communitiesResult, syncResult].find(
      (result) => result.status === "rejected",
    );
    if (failed?.status === "rejected") {
      setError(errorMessage(failed.reason, "Some Discord details could not be loaded. Try refreshing."));
    }
    setRefreshing(false);
  }, []);

  useEffect(() => {
    void refreshOverview();
    const timer = setInterval(() => {
      fetchSystemStatus().then((status) => setDiscord(status.discord)).catch(() => {});
      fetchSyncOverview()
        .then((rows) => setSync(new Map(rows.map((row) => [row.channel_id, row]))))
        .catch(() => {});
    }, 5000);
    return () => clearInterval(timer);
  }, [refreshOverview]);

  useEffect(() => {
    if (!selected) {
      setChannels(null);
      return;
    }
    setChannels(null);
    setPendingDisable(null);
    let cancelled = false;
    fetchChannels(selected)
      .then((items) => {
        if (!cancelled) setChannels(items);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(errorMessage(e, "The channels of this server could not be loaded."));
      });
    return () => {
      cancelled = true;
    };
  }, [selected]);

  // Moving focus into the confirmation keeps the keyboard where the question is.
  useEffect(() => {
    if (pendingDisable) keepArchivingRef.current?.focus();
  }, [pendingDisable]);

  const toggle = (channel: Channel) => {
    const next = !channel.archive_enabled;
    const setEnabled = (value: boolean) =>
      setChannels((current) =>
        (current ?? []).map((item) =>
          item.id === channel.id ? { ...item, archive_enabled: value } : item,
        ),
      );
    setEnabled(next);
    setChannelArchive(channel.id, next)
      .then(() => fetchSyncOverview())
      .then((rows) => setSync(new Map(rows.map((row) => [row.channel_id, row]))))
      .catch((e: unknown) => {
        setError(errorMessage(e, "The channel selection could not be saved."));
        setEnabled(!next);
      });
  };

  const selectable = (channels ?? []).filter((channel) => SELECTABLE.has(channel.kind));
  const selectedCommunity = communities?.find((community) => community.id === selected);
  const applicationID = discord?.application_id ?? "";

  return (
    <div className="page discord-page">
      <header className="page-header">
        <h1>Discord</h1>
      </header>

      {error && <div className="form-error page-error" role="alert">{error}</div>}

      <div className="card">
        <div className="card-heading-row">
          <div>
            <h2>Bot connection</h2>
            {discord === null ? (
              <span className="pill pill-neutral">Checking…</span>
            ) : discord.connected ? (
              <span className="pill pill-ok">Connected</span>
            ) : discord.configured ? (
              <span className="pill pill-bad">Disconnected</span>
            ) : (
              <span className="pill pill-neutral">Not configured</span>
            )}
          </div>
          <div className="button-row">
            {applicationID && (
              <a
                className="primary-button button-link"
                href={inviteURL(applicationID)}
                target="_blank"
                rel="noreferrer"
              >
                Add to Discord server
              </a>
            )}
            <a
              className="secondary-button button-link"
              href="https://discord.com/developers/applications"
              target="_blank"
              rel="noreferrer"
            >
              Developer portal
            </a>
          </div>
        </div>

        {discord && (
          <dl className="settings-summary">
            <div>
              <dt>Bot</dt>
              <dd>{discord.bot_username ? "@" + discord.bot_username : "—"}</dd>
            </div>
            <div>
              <dt>Application ID</dt>
              <dd>{applicationID ? <code>{applicationID}</code> : "Not configured"}</dd>
            </div>
          </dl>
        )}

        {!discord?.configured && (
          <p className="muted">
            Set <code>DISCORD_TOKEN</code> and <code>DISCORD_APPLICATION_ID</code> in your{" "}
            <code>.env</code> file, then restart OpenConvo. Bot credentials are never displayed or
            stored in the dashboard.
          </p>
        )}
        {discord?.configured && !discord.connected && (
          <div className="form-error">
            {discord.last_error ?? "OpenConvo is waiting for the Discord Gateway connection."}
          </div>
        )}
        {discord?.connected && (
          <p className="muted">
            OpenConvo is receiving Discord events. Adding the bot to a server does not archive
            anything until you explicitly enable channels below.
          </p>
        )}
        {discord?.configured && !applicationID && (
          <p className="muted">
            Set <code>DISCORD_APPLICATION_ID</code> and restart OpenConvo to enable the invite link
            and Archive message action.
          </p>
        )}
      </div>

      <div className="card">
        <h2>Required Discord access</h2>
        <ul className="discord-help-list muted">
          <li>
            Enable the privileged <em>Message Content</em> intent in the Discord developer portal.
          </li>
          <li>The invite requests only View Channels and Read Message History.</li>
          <li>Add the bot to private-channel permissions before enabling those channels.</li>
        </ul>
      </div>

      <div className="card">
        <div className="card-heading-row">
          <div>
            <h2>Servers</h2>
            <p className="muted server-heading-copy">
              Discord servers the bot can see — communities elsewhere in OpenConvo. Archived
              metadata stays here if the bot later leaves a server.
            </p>
          </div>
          <button
            type="button"
            className="secondary-button"
            onClick={() => void refreshOverview()}
            disabled={refreshing}
          >
            {refreshing ? "Refreshing…" : "Refresh"}
          </button>
        </div>

        {communities === null && <p className="muted">Loading servers…</p>}
        {communities !== null && communities.length === 0 && (
          <p className="muted">
            No server has been discovered yet. Add the bot to a server, wait for it to join, then
            refresh this page.
          </p>
        )}
        {communities !== null && communities.length > 0 && (
          <div className="server-picker">
            <label htmlFor="community-select">Server</label>
            <select
              id="community-select"
              value={selected}
              onChange={(event) => setSelected(event.target.value)}
            >
              {communities.map((community) => (
                <option key={community.id} value={community.id}>
                  {community.name}
                </option>
              ))}
            </select>
          </div>
        )}
      </div>

      {selectedCommunity && (
        <div className="card">
          <h2>Channels to archive · {selectedCommunity.name}</h2>
          <p className="muted">
            Nothing is archived until you enable it. Threads are archived with their parent
            channel.
          </p>

          {channels === null && <p className="muted">Loading channels…</p>}
          {channels !== null && selectable.length === 0 && (
            <div className="form-error">
              No archivable channels are visible. Check the bot's View Channels and Read Message
              History permissions, including channel-specific access for private channels.
            </div>
          )}
          {selectable.length > 0 && (
            <ul className="channel-list">
              {selectable.map((channel) => {
                const row = sync.get(channel.id);
                return (
                  <li key={channel.id} className="channel-row">
                    <label className="channel-toggle">
                      <input
                        type="checkbox"
                        checked={channel.archive_enabled}
                        onChange={() =>
                          channel.archive_enabled ? setPendingDisable(channel) : toggle(channel)
                        }
                      />
                      <span className="channel-name">#{channel.name}</span>
                      {channel.is_private && <span className="pill pill-neutral">private</span>}
                    </label>
                    {channel.archive_enabled && row && <SyncChip row={row} />}
                  </li>
                );
              })}
            </ul>
          )}

          {pendingDisable && (
            <div className="form-error" role="alert">
              <p>
                Stop archiving #{pendingDisable.name}? New messages and threads there will no
                longer be archived. Messages already archived are kept — nothing is deleted.
              </p>
              <div className="button-row">
                <button
                  ref={keepArchivingRef}
                  type="button"
                  className="secondary-button"
                  onClick={() => setPendingDisable(null)}
                >
                  Keep archiving
                </button>
                <button
                  type="button"
                  className="danger-button"
                  onClick={() => {
                    const channel = pendingDisable;
                    setPendingDisable(null);
                    toggle(channel);
                  }}
                >
                  Stop archiving
                </button>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
