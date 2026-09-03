import type { ArchiveAttachment, MessageSticker } from "../api";

// Some messages carry no text and still say something. Discord sends system
// events — a member joining, a pin — with an empty body and synthesizes the
// visible sentence in its own client, and a sticker replaces the body
// outright. A reader that renders only `content` shows those as blank rows.
//
// The phrases below are ours, not the author's, so callers style them apart
// from archived text. They stay deliberately plain: Discord picks a random
// greeting from a dozen for a join, and reproducing one would put words in
// the archive that nobody wrote.
const systemKinds: Record<string, string> = {
  member_join: "joined the server",
  pin: "pinned a message to this channel",
  thread_starter: "started this thread",
};

export interface TextlessMessage {
  kind: string;
  content: string | null;
  stickers: MessageSticker[];
  attachments?: ArchiveAttachment[];
}

export function stickerLabel(stickers: MessageSticker[]): string {
  const named = stickers.map((sticker) => sticker.name).filter(Boolean);
  if (named.length === 0) return stickers.length === 1 ? "Sticker" : "Stickers";
  return `Sticker: ${named.join(", ")}`;
}

// Returns what to show in place of a body, or undefined when the message
// speaks for itself — it has text, or attachments and stickers that render
// as elements of their own.
export function describeWithoutText(message: TextlessMessage): string | undefined {
  if (message.content) return undefined;
  if (message.stickers.length > 0) return undefined;

  const system = systemKinds[message.kind];
  if (system) return system;
  if (message.attachments && message.attachments.length > 0) return undefined;

  // Unmapped source message types are archived as "type_<n>" so nothing is
  // dropped. Say so rather than rendering nothing.
  const unmapped = /^type_(\d+)$/.exec(message.kind);
  if (unmapped) return `System message (type ${unmapped[1]})`;
  return "No text content";
}

// The single line shown inside a reply preview. A preview never has to say
// the message is missing: the API omits `reply_to` entirely unless the
// referenced message is archived and not deleted, so anything reaching here
// exists — it just may have nothing to quote.
export function replyPreviewText(reference: TextlessMessage): string {
  if (reference.content) return reference.content;
  if (reference.stickers.length > 0) return stickerLabel(reference.stickers);
  return describeWithoutText(reference) ?? "No text content";
}
