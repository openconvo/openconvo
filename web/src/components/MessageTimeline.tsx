import { useEffect, useState, type MouseEvent } from "react";
import { Link } from "react-router-dom";
import {
  attachmentContentURL,
  createBookmark,
  downloadAttachment,
  formatBytes,
  type ArchiveAttachment,
  type ArchiveMessage,
} from "../api";
import { describeWithoutText, replyPreviewText, stickerLabel } from "./messageSummary";

const dateTime = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});

function actorName(message: ArchiveMessage): string {
  return message.actor?.display_name || message.actor?.username || "Unknown user";
}

function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  return parts
    .slice(0, 2)
    .map((part) => Array.from(part)[0])
    .join("")
    .toLocaleUpperCase();
}

function Attachment({
  attachment,
  onError,
}: {
  attachment: ArchiveAttachment;
  onError: (message: string) => void;
}) {
  const details = [formatBytes(attachment.size), attachment.content_type].filter(Boolean).join(" · ");
  const download = (event: MouseEvent<HTMLAnchorElement>) => {
    event.preventDefault();
    downloadAttachment(attachment.id).catch((cause: unknown) =>
      onError(cause instanceof Error ? cause.message : String(cause)),
    );
  };
  return (
    <li className="message-attachment">
      <span className="attachment-icon" aria-hidden="true">↧</span>
      <span>
        {attachment.download_status === "stored" ? (
          <a href={attachmentContentURL(attachment.id)} onClick={download}>
            {attachment.filename || "attachment"}
          </a>
        ) : (
          <span>{attachment.filename || "attachment"}</span>
        )}
        {attachment.description && <span className="attachment-description">{attachment.description}</span>}
        <span className="attachment-meta">
          {details}
          {attachment.download_status === "pending" && " · referenced, not stored yet"}
          {attachment.download_status === "failed" && " · unavailable at source"}
        </span>
      </span>
    </li>
  );
}

export default function MessageTimeline({
  messages,
  targetId,
}: {
  messages: ArchiveMessage[];
  targetId?: string;
}) {
  const [saved, setSaved] = useState<Record<string, string>>(() => Object.fromEntries(
    messages.filter((message) => message.bookmark_id).map((message) => [message.id, message.bookmark_id!]),
  ));
  const [saving, setSaving] = useState<string>();
  const [actionError, setActionError] = useState<string>();

  useEffect(() => {
    setSaved((current) => {
      const next = { ...current };
      for (const message of messages) {
        if (message.bookmark_id) next[message.id] = message.bookmark_id;
      }
      return next;
    });
  }, [messages]);

  const save = async (messageId: string) => {
    if (saved[messageId] || saving) return;
    setSaving(messageId);
    setActionError(undefined);
    try {
      const bookmark = await createBookmark(messageId);
      setSaved((current) => ({ ...current, [messageId]: bookmark.id }));
    } catch (error) {
      setActionError(error instanceof Error ? error.message : String(error));
    } finally {
      setSaving(undefined);
    }
  };

  return (
    <div className="message-timeline">
      {actionError && <div className="timeline-error" role="alert">{actionError}</div>}
      {messages.map((message) => {
        const name = actorName(message);
        const created = new Date(message.source_created_at);
        const described = describeWithoutText(message);
        return (
          <article
            id={`message-${message.id}`}
            key={message.id}
            className={message.id === targetId ? "message message-target" : "message"}
          >
            <div className="message-avatar" aria-hidden="true">{initials(name)}</div>
            <div className="message-body">
              {message.reply_to && (
                <Link className="reply-preview" to={`/messages/${message.reply_to.id}`}>
                  <span aria-hidden="true">↳</span>
                  <strong>
                    {message.reply_to.actor?.display_name ||
                      message.reply_to.actor?.username ||
                      "Unknown user"}
                  </strong>
                  <span>{replyPreviewText(message.reply_to)}</span>
                </Link>
              )}
              <div className="message-heading">
                <strong>{name}</strong>
                {message.actor?.is_bot && <span className="bot-label">bot</span>}
                <Link
                  className="message-time"
                  to={`/messages/${message.id}`}
                  title={created.toLocaleString()}
                >
                  {dateTime.format(created)}
                </Link>
                {message.source_updated_at && <span className="edited-label">edited</span>}
                {saved[message.id] ? (
                  <Link className="bookmark-action saved" to="/bookmarks">Saved</Link>
                ) : (
                  <button
                    type="button"
                    className="bookmark-action"
                    disabled={saving === message.id}
                    onClick={() => void save(message.id)}
                  >
                    {saving === message.id ? "Saving…" : "Save"}
                  </button>
                )}
              </div>
              {message.content && <div className="message-content">{message.content}</div>}
              {described && <div className="message-synthesized">{described}</div>}
              {message.stickers.length > 0 && (
                <div className="message-stickers">
                  <span className="sticker">
                    <span aria-hidden="true">☺</span> {stickerLabel(message.stickers)}
                  </span>
                </div>
              )}
              {message.attachments.length > 0 && (
                <ul className="message-attachments">
                  {message.attachments.map((attachment) => (
                    <Attachment
                      key={attachment.id}
                      attachment={attachment}
                      onError={setActionError}
                    />
                  ))}
                </ul>
              )}
              {message.reactions.length > 0 && (
                <div className="message-reactions" aria-label="Reactions">
                  {message.reactions.map((reaction) => (
                    <span key={reaction.id} className="reaction">
                      {reaction.emoji_name || reaction.emoji_key} {reaction.count}
                    </span>
                  ))}
                </div>
              )}
            </div>
          </article>
        );
      })}
    </div>
  );
}
