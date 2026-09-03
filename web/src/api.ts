// Typed client for the OpenConvo API (/api/v1).

export interface VersionInfo {
  version: string;
  commit: string;
  date: string;
  go_version: string;
}

export interface DatabaseStatus {
  connected: boolean;
  schema_version?: number;
  error?: string;
}

export interface StorageStatus {
  driver: string;
  path?: string;
}

export interface DiscordStatus {
  configured: boolean;
  connected: boolean;
  application_id?: string;
  bot_username?: string;
  last_error?: string;
}

export interface CountsStatus {
  communities: number;
  channels: number;
  messages: number;
  attachments: number;
}

export interface AttachmentsStatus {
  enabled: boolean;
  stored: number;
  pending: number;
  failed: number;
  stored_bytes: number;
}

export interface SystemStatus {
  version: VersionInfo;
  started_at: string;
  database: DatabaseStatus;
  storage: StorageStatus;
  discord: DiscordStatus;
  attachments?: AttachmentsStatus;
  counts?: CountsStatus;
  insecure_public_access: boolean;
}

export interface UpdateStatus {
  current_version: string;
  latest_version?: string;
  update_available: boolean;
  command_upgrade_allowed: boolean;
  reason:
    | "up-to-date"
    | "update-available"
    | "manual-upgrade-required"
    | "development-build";
  release_url?: string;
  published_at?: string;
  checked_at: string;
  upgrade_command?: string;
}

export interface AuthSession {
  authenticated: boolean;
}

export interface Community {
  id: string;
  source: string;
  external_id: string;
  name: string;
  icon_url: string;
}

export interface Channel {
  id: string;
  community_id: string;
  external_id: string;
  parent_channel_id?: string;
  kind: string;
  name: string;
  topic: string;
  position: number;
  is_private: boolean;
  is_archived: boolean;
  archive_enabled: boolean;
}

export interface SyncRow {
  channel_id: string;
  channel_name: string;
  community_name: string;
  kind: string;
  status: string;
  backfill_complete: boolean;
  last_synced_at?: string;
  last_error?: string;
  message_count: number;
}

export interface ArchiveChannel {
  id: string;
  community_id: string;
  community_name: string;
  parent_channel_id?: string;
  parent_channel_name?: string;
  parent_kind?: string;
  kind: string;
  name: string;
  topic: string;
  position: number;
  is_private: boolean;
  is_archived: boolean;
  archive_enabled: boolean;
  sync_status: string;
  backfill_complete: boolean;
  message_count: number;
  last_message_at?: string;
}

export interface ArchiveActor {
  id: string;
  username: string;
  display_name: string;
  avatar_url?: string;
  is_bot: boolean;
}

export interface ArchiveAttachment {
  id: string;
  filename: string;
  description?: string;
  content_type?: string;
  size: number;
  download_status: "pending" | "stored" | "failed";
}

export interface Reaction {
  id: string;
  message_id: string;
  emoji_key: string;
  emoji_name: string;
  count: number;
}

export interface MessageSticker {
  id: string;
  name: string;
}

export interface MessageReference {
  id: string;
  kind: string;
  content: string | null;
  stickers: MessageSticker[];
  actor?: ArchiveActor;
  source_created_at: string;
}

export interface ArchiveMessage {
  id: string;
  channel_id: string;
  external_id: string;
  kind: string;
  content: string | null;
  stickers: MessageSticker[];
  actor?: ArchiveActor;
  reply_to?: MessageReference;
  source_created_at: string;
  source_updated_at?: string;
  attachments: ArchiveAttachment[];
  reactions: Reaction[];
  bookmark_id?: string;
}

export interface Bookmark {
  id: string;
  message_id: string;
  title: string;
  description: string;
  tags: string[];
  collection: string;
  channel_id: string;
  channel_name: string;
  community_name: string;
  content: string | null;
  actor?: ArchiveActor;
  source_created_at: string;
  created_at: string;
  updated_at: string;
}

export interface BookmarkInput {
  title: string;
  description: string;
  tags: string[];
  collection: string;
}

export interface MessagePage {
  channel: ArchiveChannel;
  messages: ArchiveMessage[];
  has_older: boolean;
}

export interface MessageContext {
  channel: ArchiveChannel;
  target_id: string;
  messages: ArchiveMessage[];
}

export interface SearchResult {
  message_id: string;
  channel_id: string;
  channel_name: string;
  community_name: string;
  actor?: ArchiveActor;
  source_created_at: string;
  excerpt: string;
  has_attachment: boolean;
}

export interface SearchPage {
  results: SearchResult[];
  has_more: boolean;
}

export interface SearchOptions {
  query: string;
  mode?: "keyword" | "semantic";
  channelId?: string;
  author?: string;
  after?: string;
  before?: string;
  hasAttachment?: boolean;
  limit?: number;
  offset?: number;
}

export type BackupProvider = "s3" | "r2" | "backblaze" | "custom";

export interface BackupSettings {
  enabled: boolean;
  provider: BackupProvider;
  endpoint: string;
  region: string;
  bucket: string;
  prefix: string;
  force_path_style: boolean;
  interval_hours: number;
  retention_count: number;
  credentials_configured: boolean;
  source: "environment" | "dashboard";
}

export interface DatabaseBackup {
  id: string;
  trigger: "manual" | "scheduled";
  status: "pending" | "running" | "succeeded" | "failed";
  provider: BackupProvider;
  endpoint?: string;
  region: string;
  bucket: string;
  prefix: string;
  object_key: string;
  size: number;
  sha256?: string;
  error?: string;
  started_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
  download_available: boolean;
}

// BackupRequest is the answer to "back up now": RequestBackup returns the
// backup that is already running, with created false, when one is in flight.
export interface BackupRequest {
  backup: DatabaseBackup;
  created: boolean;
}

export interface EmbeddingSettings {
  enabled: boolean;
  provider: "openai";
  model: "text-embedding-3-small";
  dimensions: 256;
  input_version: "message-content-v1";
  credentials_configured: boolean;
  source: "environment" | "dashboard";
  generation_status?: "building" | "active" | "retired";
  embedded_messages: number;
  eligible_messages: number;
}

// formatBytes renders the byte counts the API reports — attachment sizes,
// stored bytes, dump sizes — so every screen agrees on the units.
export function formatBytes(bytes: number): string {
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

async function apiFetch(url: string, init?: RequestInit): Promise<Response> {
  const res = await fetch(url, { credentials: "same-origin", ...init });
  if (res.status === 401) {
    window.dispatchEvent(new Event("openconvo:authentication-required"));
  }
  return res;
}

// Every API error path answers with {"error": "..."}, a message written for
// the operator: why a channel cannot be selected, how long a rate limit
// lasts. Use it, and fall back to the status only when the body is not that
// JSON — an empty body, a proxy's HTML error page, a HEAD response.
async function responseError(res: Response, fallback: string): Promise<Error> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body.error) return new Error(body.error);
  } catch {
    // Use the stable fallback when the server did not return JSON.
  }
  return new Error(`${fallback}: HTTP ${res.status}`);
}

async function getJSON<T>(url: string): Promise<T> {
  const res = await apiFetch(url, { headers: { Accept: "application/json" } });
  if (!res.ok) throw await responseError(res, url);
  return (await res.json()) as T;
}

// Attachment bytes and database dumps stream from the server rather than
// arriving as JSON, so the browser downloads them itself. The HEAD check
// still goes through this client: it shares the download route's
// authentication and availability checks, so an expired session reaches the
// same handler as every other call, and a failure is reported in the UI
// instead of arriving as a downloaded file full of JSON.
async function startDownload(url: string, fallback: string): Promise<void> {
  const res = await apiFetch(url, { method: "HEAD" });
  if (!res.ok) throw await responseError(res, fallback);
  const link = document.createElement("a");
  link.href = url;
  // download also keeps a late failure from replacing the page; the server's
  // Content-Disposition still names the file.
  link.download = "";
  link.rel = "noopener";
  document.body.appendChild(link);
  link.click();
  link.remove();
}

export function attachmentContentURL(attachmentId: string): string {
  return `/api/v1/attachments/${encodeURIComponent(attachmentId)}/content`;
}

export async function downloadAttachment(attachmentId: string): Promise<void> {
  return startDownload(attachmentContentURL(attachmentId), "downloading the attachment failed");
}

export function backupContentURL(backupId: string): string {
  return `/api/v1/backups/${encodeURIComponent(backupId)}/content`;
}

export async function downloadDatabaseBackup(backupId: string): Promise<void> {
  return startDownload(backupContentURL(backupId), "downloading the database backup failed");
}

// The three session routes sit outside the authenticated API, so they call
// fetch directly: a 401 here means the submitted password was wrong, not that
// a session expired, and must not push the app back to the sign-in screen it
// is already showing.
export async function fetchAuthSession(): Promise<AuthSession> {
  const res = await fetch("/api/v1/auth/session", {
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) throw await responseError(res, "session request failed");
  return (await res.json()) as AuthSession;
}

export async function login(password: string): Promise<void> {
  const res = await fetch("/api/v1/auth/session", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({ password }),
  });
  if (!res.ok) throw await responseError(res, "sign-in failed");
}

export async function logout(): Promise<void> {
  const res = await fetch("/api/v1/auth/session", {
    method: "DELETE",
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) throw await responseError(res, "sign-out failed");
}

export async function fetchSystemStatus(): Promise<SystemStatus> {
  const res = await apiFetch("/api/v1/system/status", {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) throw await responseError(res, "status request failed");
  return (await res.json()) as SystemStatus;
}

export async function fetchUpdateStatus(): Promise<UpdateStatus> {
  return getJSON<UpdateStatus>("/api/v1/system/update");
}

export async function fetchCommunities(): Promise<Community[]> {
  return (await getJSON<{ communities: Community[] }>("/api/v1/communities")).communities;
}

export async function fetchChannels(communityId: string): Promise<Channel[]> {
  return (
    await getJSON<{ channels: Channel[] }>(
      `/api/v1/communities/${encodeURIComponent(communityId)}/channels`,
    )
  ).channels;
}

export async function setChannelArchive(channelId: string, enabled: boolean): Promise<Channel> {
  const res = await apiFetch(`/api/v1/channels/${encodeURIComponent(channelId)}/archive`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({ enabled }),
  });
  if (!res.ok) throw await responseError(res, "toggle failed");
  return ((await res.json()) as { channel: Channel }).channel;
}

export async function fetchSyncOverview(): Promise<SyncRow[]> {
  return (await getJSON<{ channels: SyncRow[] }>("/api/v1/system/sync")).channels;
}

export async function fetchArchiveChannels(): Promise<ArchiveChannel[]> {
  return (await getJSON<{ channels: ArchiveChannel[] }>("/api/v1/channels")).channels;
}

export async function fetchChannelMessages(
  channelId: string,
  before?: string,
  limit = 50,
): Promise<MessagePage> {
  const query = new URLSearchParams({ limit: String(limit) });
  if (before) query.set("before", before);
  return getJSON<MessagePage>(
    `/api/v1/channels/${encodeURIComponent(channelId)}/messages?${query.toString()}`,
  );
}

export async function fetchMessageContext(messageId: string): Promise<MessageContext> {
  return getJSON<MessageContext>(`/api/v1/messages/${encodeURIComponent(messageId)}`);
}

export async function searchMessages(options: SearchOptions): Promise<SearchPage> {
  const query = new URLSearchParams({ q: options.query });
  if (options.mode === "semantic") query.set("mode", "semantic");
  if (options.channelId) query.set("channel_id", options.channelId);
  if (options.author) query.set("author", options.author);
  if (options.after) query.set("after", options.after);
  if (options.before) query.set("before", options.before);
  if (options.hasAttachment) query.set("has_attachment", "true");
  if (options.limit !== undefined) query.set("limit", String(options.limit));
  if (options.offset !== undefined) query.set("offset", String(options.offset));
  const res = await apiFetch(`/api/v1/search?${query.toString()}`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) throw await responseError(res, "searching messages failed");
  return (await res.json()) as SearchPage;
}

export async function fetchBookmarks(collection = "", tag = ""): Promise<Bookmark[]> {
  const query = new URLSearchParams();
  if (collection) query.set("collection", collection);
  if (tag) query.set("tag", tag);
  const suffix = query.size > 0 ? `?${query.toString()}` : "";
  return (await getJSON<{ bookmarks: Bookmark[] }>(`/api/v1/bookmarks${suffix}`)).bookmarks;
}

export async function createBookmark(messageId: string): Promise<Bookmark> {
  const res = await apiFetch("/api/v1/bookmarks", {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({
      message_id: messageId, title: "", description: "", tags: [], collection: "",
    }),
  });
  if (!res.ok) throw await responseError(res, "save failed");
  return ((await res.json()) as { bookmark: Bookmark }).bookmark;
}

export async function updateBookmark(id: string, input: BookmarkInput): Promise<Bookmark> {
  const res = await apiFetch(`/api/v1/bookmarks/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(input),
  });
  if (!res.ok) throw await responseError(res, "update failed");
  return ((await res.json()) as { bookmark: Bookmark }).bookmark;
}

export async function deleteBookmark(id: string): Promise<void> {
  const res = await apiFetch(`/api/v1/bookmarks/${encodeURIComponent(id)}`, {
    method: "DELETE",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) throw await responseError(res, "delete failed");
}

export async function fetchBackupSettings(): Promise<BackupSettings> {
  return getJSON<BackupSettings>("/api/v1/backups/settings");
}

export async function saveBackupSettings(settings: BackupSettings): Promise<BackupSettings> {
  const { credentials_configured: _credentials, source: _source, ...input } = settings;
  const res = await apiFetch("/api/v1/backups/settings", {
    method: "PUT",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(input),
  });
  if (!res.ok) throw await responseError(res, "saving backup settings failed");
  return (await res.json()) as BackupSettings;
}

export async function fetchEmbeddingSettings(): Promise<EmbeddingSettings> {
  return getJSON<EmbeddingSettings>("/api/v1/embeddings/settings");
}

export async function saveEmbeddingSettings(settings: EmbeddingSettings): Promise<EmbeddingSettings> {
  const {
    credentials_configured: _credentials,
    source: _source,
    generation_status: _status,
    embedded_messages: _embedded,
    eligible_messages: _eligible,
    ...input
  } = settings;
  const res = await apiFetch("/api/v1/embeddings/settings", {
    method: "PUT",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(input),
  });
  if (!res.ok) throw await responseError(res, "saving embedding settings failed");
  return (await res.json()) as EmbeddingSettings;
}

export async function fetchDatabaseBackups(): Promise<DatabaseBackup[]> {
  return (await getJSON<{ backups: DatabaseBackup[] }>("/api/v1/backups")).backups;
}

export async function createDatabaseBackup(): Promise<BackupRequest> {
  const res = await apiFetch("/api/v1/backups", {
    method: "POST",
    headers: { Accept: "application/json" },
  });
  if (!res.ok) throw await responseError(res, "starting database backup failed");
  return (await res.json()) as BackupRequest;
}
