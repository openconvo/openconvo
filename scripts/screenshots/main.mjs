// Render the real OpenConvo frontend against a fixed, synthetic API and
// capture documentation images with headless Chrome. No community archive or
// credentials are read.
import { createRequire } from "node:module";
import { createServer } from "node:http";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, statSync } from "node:fs";
import { extname, join, resolve, sep } from "node:path";

const requireFromWeb = createRequire(resolve("web/package.json"));
const { chromium } = requireFromWeb("playwright-core");

const outputDir = resolve("docs/images");
const distDir = resolve("internal/web/dist");

const shots = [
  { name: "archive", path: "/channels/chan-workshop", height: 1000, ready: "#workshop" },
  { name: "search", path: "/search?q=pressure", height: 1050, ready: "3 results" },
  { name: "bookmarks", path: "/bookmarks", height: 1200, ready: "2 of 2" },
  { name: "dashboard", path: "/", height: 1100, ready: "18,426" },
  { name: "discord", path: "/discord", height: 1200, ready: "Channels to archive · Field Notes Collective" },
  { name: "backups", path: "/backups", height: 1200, ready: "Download" },
];

async function captureScreenshots() {
  if (!existsSync(join(distDir, "index.html"))) {
    throw new Error("frontend is not built; run make web first");
  }
  mkdirSync(outputDir, { recursive: true });

  const server = createServer((request, response) => {
    const url = new URL(request.url ?? "/", "http://openconvo.invalid");
    if (url.pathname.startsWith("/api/v1/")) {
      serveAPI(url.pathname, response);
      return;
    }
    serveFrontend(url.pathname, response);
  });

  await new Promise((resolveListen, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolveListen);
  });

  const address = server.address();
  if (!address || typeof address === "string") throw new Error("screenshot server did not bind a TCP port");
  const baseURL = `http://127.0.0.1:${address.port}`;

  const browser = await chromium.launch({
    executablePath: findChrome(),
    headless: true,
    args: ["--disable-background-networking", "--force-color-profile=srgb"],
  });

  try {
    for (const shot of shots) {
      const page = await browser.newPage({
        viewport: { width: 1440, height: shot.height },
        deviceScaleFactor: 1,
        colorScheme: "light",
        locale: "en-AU",
        timezoneId: "Australia/Melbourne",
      });
      await page.goto(baseURL + shot.path, { waitUntil: "networkidle" });
      await page.getByText(shot.ready, { exact: true }).first().waitFor();
      const output = join(outputDir, `${shot.name}.png`);
      await page.screenshot({ path: output, animations: "disabled" });
      await page.close();
      console.log(`wrote ${output} (${statSync(output).size} bytes)`);
    }
  } finally {
    await browser.close();
    await new Promise((resolveClose, reject) => server.close((error) => error ? reject(error) : resolveClose()));
  }
}

function findChrome() {
  if (process.env.OPENCONVO_SCREENSHOT_CHROME) {
    if (!existsSync(process.env.OPENCONVO_SCREENSHOT_CHROME)) {
      throw new Error(`OPENCONVO_SCREENSHOT_CHROME does not exist: ${process.env.OPENCONVO_SCREENSHOT_CHROME}`);
    }
    return process.env.OPENCONVO_SCREENSHOT_CHROME;
  }

  const fixed = process.platform === "darwin"
    ? [
        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
        "/Applications/Chromium.app/Contents/MacOS/Chromium",
      ]
    : process.platform === "win32"
      ? [
          join(process.env.PROGRAMFILES ?? "", "Google/Chrome/Application/chrome.exe"),
          join(process.env["PROGRAMFILES(X86)"] ?? "", "Google/Chrome/Application/chrome.exe"),
        ]
      : [];
  for (const candidate of fixed) {
    if (candidate && existsSync(candidate)) return candidate;
  }

  for (const name of ["google-chrome", "google-chrome-stable", "chromium", "chromium-browser"]) {
    try {
      return execFileSync(process.platform === "win32" ? "where" : "which", [name], { encoding: "utf8" }).trim().split("\n")[0];
    } catch {
      // Try the next conventional executable name.
    }
  }
  throw new Error("Chrome or Chromium not found; set OPENCONVO_SCREENSHOT_CHROME to its executable");
}

function serveFrontend(pathname, response) {
  const requested = pathname === "/" ? "index.html" : decodeURIComponent(pathname.slice(1));
  let filename = resolve(distDir, requested);
  if (!filename.startsWith(distDir + sep) || !existsSync(filename) || !statSync(filename).isFile()) {
    filename = join(distDir, "index.html");
  }
  const contentTypes = {
    ".css": "text/css; charset=utf-8",
    ".html": "text/html; charset=utf-8",
    ".js": "text/javascript; charset=utf-8",
    ".svg": "image/svg+xml",
  };
  response.writeHead(200, { "Content-Type": contentTypes[extname(filename)] ?? "application/octet-stream" });
  response.end(readFileSync(filename));
}

function serveAPI(pathname, response) {
  let document;
  if (pathname === "/api/v1/auth/session") document = auth;
  else if (pathname === "/api/v1/system/status") document = status;
  else if (pathname === "/api/v1/system/sync") document = sync;
  else if (pathname === "/api/v1/system/update") document = update;
  else if (pathname === "/api/v1/channels") document = { channels };
  else if (pathname.startsWith("/api/v1/channels/") && pathname.endsWith("/messages")) document = messagePage;
  else if (pathname === "/api/v1/search") document = search;
  else if (pathname === "/api/v1/bookmarks") document = { bookmarks };
  else if (pathname === "/api/v1/backups/settings") document = backupSettings;
  else if (pathname === "/api/v1/backups") document = { backups };
  else if (pathname === "/api/v1/communities") document = { communities };
  else if (pathname.startsWith("/api/v1/communities/") && pathname.endsWith("/channels")) document = { channels: discordChannels };
  else {
    response.writeHead(404, { "Content-Type": "application/json" });
    response.end(JSON.stringify({ error: "synthetic screenshot endpoint not found" }));
    return;
  }
  response.writeHead(200, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  response.end(JSON.stringify(document));
}

const auth = { authenticated: true };
const maya = { id: "actor-maya", username: "mayac", display_name: "Maya Chen", is_bot: false };
const samir = { id: "actor-samir", username: "samir_r", display_name: "Samir Rao", is_bot: false };
const ellis = { id: "actor-ellis", username: "ellisnorth", display_name: "Ellis North", is_bot: false };

const status = {
  version: { version: "0.1.0", commit: "8f21c746d1", date: "2026-08-27T04:00:00Z", go_version: "go1.26.6" },
  started_at: "2026-08-24T01:15:00Z",
  database: { connected: true, schema_version: 2 },
  storage: { driver: "s3" },
  discord: { configured: true, connected: true, application_id: "1483927164052738048", bot_username: "OpenConvo Archive" },
  attachments: { enabled: true, stored: 614, pending: 0, failed: 0, stored_bytes: 4831838208 },
  counts: { communities: 1, channels: 14, messages: 18426, attachments: 614 },
  insecure_public_access: false,
};

const update = {
  current_version: "0.1.0", latest_version: "0.1.0", update_available: false,
  command_upgrade_allowed: true, reason: "up-to-date", checked_at: "2026-08-28T09:35:00Z",
};

const sync = { channels: [
  { channel_id: "chan-workshop", channel_name: "workshop", community_name: "Field Notes Collective", kind: "text", status: "synced", backfill_complete: true, last_synced_at: "2026-08-28T09:31:00Z", message_count: 18426 },
  { channel_id: "chan-resources", channel_name: "resources", community_name: "Field Notes Collective", kind: "forum", status: "synced", backfill_complete: true, last_synced_at: "2026-08-28T09:29:00Z", message_count: 3912 },
  { channel_id: "chan-announcements", channel_name: "announcements", community_name: "Field Notes Collective", kind: "announcement", status: "importing", backfill_complete: false, last_synced_at: "2026-08-28T09:28:00Z", message_count: 847 },
] };

const workshop = {
  id: "chan-workshop", community_id: "community-fieldnotes", community_name: "Field Notes Collective",
  kind: "text", name: "workshop", topic: "Projects in progress, practical fixes, and lessons worth keeping.",
  position: 3, is_private: false, is_archived: false, archive_enabled: true, sync_status: "synced",
  backfill_complete: true, message_count: 18426, last_message_at: "2026-08-28T09:31:00Z",
};

const channels = [
  workshop,
  { id: "chan-resources", community_id: "community-fieldnotes", community_name: "Field Notes Collective", kind: "forum", name: "resources", topic: "Reference material and trusted suppliers.", position: 4, is_private: false, is_archived: false, archive_enabled: true, sync_status: "synced", backfill_complete: true, message_count: 3912, last_message_at: "2026-08-28T09:29:00Z" },
  { id: "thread-rainwater", community_id: "community-fieldnotes", community_name: "Field Notes Collective", parent_channel_id: "chan-workshop", kind: "public_thread", name: "Rainwater pump losing pressure", topic: "", position: 0, is_private: false, is_archived: false, archive_enabled: false, sync_status: "synced", backfill_complete: true, message_count: 23, last_message_at: "2026-08-28T09:31:00Z" },
];

const messages = [
  { id: "msg-1", channel_id: workshop.id, external_id: "9001", kind: "default", content: "Has anyone solved short-cycling on a rainwater pump? Ours starts every few seconds even when every tap is closed. The pressure tank reads 42 psi.", stickers: [], actor: maya, source_created_at: "2026-08-28T08:42:00Z", attachments: [], reactions: [] },
  { id: "msg-2", channel_id: workshop.id, external_id: "9002", kind: "default", content: "That usually means the tank has lost its air charge. Switch the pump off, open a tap until the gauge reaches zero, then check the Schrader valve. Set it 2 psi below the cut-in pressure before restarting.", stickers: [], actor: samir, reply_to: { id: "msg-1", kind: "default", content: "Has anyone solved short-cycling on a rainwater pump? Ours starts every few seconds even when every tap is closed.", stickers: [], actor: maya, source_created_at: "2026-08-28T08:42:00Z" }, source_created_at: "2026-08-28T08:49:00Z", source_updated_at: "2026-08-28T08:52:00Z", attachments: [], reactions: [{ id: "react-1", message_id: "msg-2", emoji_key: "👍", emoji_name: "👍", count: 8 }, { id: "react-2", message_id: "msg-2", emoji_key: "🔧", emoji_name: "🔧", count: 3 }], bookmark_id: "bookmark-1" },
  { id: "msg-3", channel_id: workshop.id, external_id: "9003", kind: "default", content: "Cut-in is 30 psi, and the tank was down at 9. Recharged it to 28 and the cycling stopped immediately. I wrote the steps on the pump cabinet for next time.", stickers: [], actor: maya, source_created_at: "2026-08-28T09:18:00Z", attachments: [{ id: "att-1", filename: "pressure-switch-notes.pdf", description: "Settings and restart checklist", content_type: "application/pdf", size: 184320, download_status: "stored" }], reactions: [{ id: "react-3", message_id: "msg-3", emoji_key: "🎉", emoji_name: "🎉", count: 6 }] },
  { id: "msg-4", channel_id: workshop.id, external_id: "9004", kind: "default", content: "For the archive: if water comes out of the Schrader valve, the diaphragm has failed and recharging will only be temporary. Replace the tank.", stickers: [], actor: ellis, source_created_at: "2026-08-28T09:31:00Z", attachments: [], reactions: [{ id: "react-4", message_id: "msg-4", emoji_key: "custom:1:saved", emoji_name: "saved", count: 4 }] },
];
const messagePage = { channel: workshop, messages, has_older: true };

const search = { has_more: false, results: [
  { message_id: "msg-2", channel_id: workshop.id, channel_name: "workshop", community_name: "Field Notes Collective", actor: samir, source_created_at: "2026-08-28T08:49:00Z", excerpt: "That usually means the tank has lost its air charge. Switch the pump off, open a tap until the <mark>pressure</mark> gauge reaches zero, then check the Schrader valve.", has_attachment: false },
  { message_id: "msg-3", channel_id: workshop.id, channel_name: "workshop", community_name: "Field Notes Collective", actor: maya, source_created_at: "2026-08-28T09:18:00Z", excerpt: "Cut-in is 30 psi, and the tank was down at 9. Recharged it to 28 and the cycling stopped immediately.", has_attachment: true },
  { message_id: "msg-5", channel_id: "chan-resources", channel_name: "resources", community_name: "Field Notes Collective", actor: ellis, source_created_at: "2026-04-12T03:22:00Z", excerpt: "A useful reference for sizing a <mark>pressure</mark> tank: start with pump flow, desired runtime, and the cut-in/cut-out range.", has_attachment: true },
] };

const bookmarks = [
  { id: "bookmark-1", message_id: "msg-2", title: "Recharging a water pressure tank", description: "Fast diagnostic for short-cycling, including the correct pre-charge relative to cut-in pressure.", tags: ["water", "maintenance", "diagnostics"], collection: "Workshop fixes", channel_id: workshop.id, channel_name: "workshop", community_name: "Field Notes Collective", content: messages[1].content, actor: samir, source_created_at: messages[1].source_created_at, created_at: "2026-08-28T09:04:00Z", updated_at: "2026-08-28T09:06:00Z" },
  { id: "bookmark-2", message_id: "msg-6", title: "Outdoor workbench finish schedule", description: "The finish combination that survived a full wet season without lifting at the end grain.", tags: ["timber", "finishing", "outdoors"], collection: "Build methods", channel_id: workshop.id, channel_name: "workshop", community_name: "Field Notes Collective", content: "Two coats of penetrating epoxy on the end grain, then three thin coats of exterior oil. Leave the underside unsealed so trapped moisture still has somewhere to go.", actor: { id: "actor-jo", username: "jo_builds", display_name: "Jo Alvarez", is_bot: false }, source_created_at: "2026-06-03T05:12:00Z", created_at: "2026-06-03T06:40:00Z", updated_at: "2026-06-03T06:40:00Z" },
];

const backupSettings = {
  enabled: true, provider: "r2", endpoint: "https://8f2c1d0a.r2.cloudflarestorage.com", region: "auto",
  bucket: "field-notes-openconvo", prefix: "openconvo/database-backups", force_path_style: false,
  interval_hours: 24, retention_count: 30, credentials_configured: true, source: "dashboard",
};

const backups = [
  { id: "backup-1", trigger: "scheduled", status: "succeeded", provider: "r2", region: "auto", bucket: backupSettings.bucket, prefix: backupSettings.prefix, object_key: `${backupSettings.prefix}/openconvo-2026-08-28T020000Z.dump`, size: 68157440, sha256: "30ab3f22", started_at: "2026-08-28T02:00:00Z", completed_at: "2026-08-28T02:01:18Z", created_at: "2026-08-28T02:00:00Z", updated_at: "2026-08-28T02:01:18Z", download_available: true },
  { id: "backup-2", trigger: "manual", status: "succeeded", provider: "r2", region: "auto", bucket: backupSettings.bucket, prefix: backupSettings.prefix, object_key: `${backupSettings.prefix}/openconvo-2026-08-27T081400Z.dump`, size: 67371008, sha256: "59d1f304", started_at: "2026-08-27T08:14:00Z", completed_at: "2026-08-27T08:15:11Z", created_at: "2026-08-27T08:14:00Z", updated_at: "2026-08-27T08:15:11Z", download_available: true },
  { id: "backup-3", trigger: "scheduled", status: "succeeded", provider: "r2", region: "auto", bucket: backupSettings.bucket, prefix: backupSettings.prefix, object_key: `${backupSettings.prefix}/openconvo-2026-08-27T020000Z.dump`, size: 67108864, sha256: "e7a092c1", started_at: "2026-08-27T02:00:00Z", completed_at: "2026-08-27T02:01:09Z", created_at: "2026-08-27T02:00:00Z", updated_at: "2026-08-27T02:01:09Z", download_available: true },
];

const communities = [
  { id: "community-fieldnotes", source: "discord", external_id: "824613097214663721", name: "Field Notes Collective", icon_url: "" },
];

const discordChannels = [
  { id: "chan-announcements", community_id: communities[0].id, external_id: "1001", kind: "announcement", name: "announcements", topic: "News from the community", position: 1, is_private: false, is_archived: false, archive_enabled: true },
  { id: workshop.id, community_id: communities[0].id, external_id: "1002", kind: "text", name: "workshop", topic: workshop.topic, position: 2, is_private: false, is_archived: false, archive_enabled: true },
  { id: "chan-resources", community_id: communities[0].id, external_id: "1003", kind: "forum", name: "resources", topic: "Reference material and trusted suppliers", position: 3, is_private: false, is_archived: false, archive_enabled: true },
  { id: "chan-mods", community_id: communities[0].id, external_id: "1004", kind: "text", name: "moderators", topic: "Private coordination", position: 4, is_private: true, is_archived: false, archive_enabled: false },
  { id: "chan-offtopic", community_id: communities[0].id, external_id: "1005", kind: "text", name: "off-topic", topic: "Everything else", position: 5, is_private: false, is_archived: false, archive_enabled: false },
];

await captureScreenshots();
