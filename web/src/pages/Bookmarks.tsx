import { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import {
  deleteBookmark,
  fetchBookmarks,
  updateBookmark,
  type Bookmark,
  type BookmarkInput,
} from "../api";
import { errorMessage } from "./errorMessage";

// Mirrors the tag limits internal/http/handlers.go enforces, so an overlong
// or overlong-list edit is explained here instead of coming back as a 400.
const maxTags = 20;
const maxTagLength = 50;

const dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

function displayActor(bookmark: Bookmark): string {
  return bookmark.actor?.display_name || bookmark.actor?.username || "Unknown user";
}

function BookmarkCard({
  bookmark,
  onUpdated,
  onDeleted,
}: {
  bookmark: Bookmark;
  onUpdated: (bookmark: Bookmark) => void;
  onDeleted: (id: string) => void;
}) {
  const [draft, setDraft] = useState<BookmarkInput>({
    title: bookmark.title,
    description: bookmark.description,
    tags: bookmark.tags,
    collection: bookmark.collection,
  });
  const [tags, setTags] = useState(bookmark.tags.join(", "));
  const [phase, setPhase] = useState<"idle" | "saving" | "deleting">("idle");
  const [confirmingRemove, setConfirmingRemove] = useState(false);
  const [error, setError] = useState<string>();
  const keepRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (confirmingRemove) keepRef.current?.focus();
  }, [confirmingRemove]);

  const save = async () => {
    const parsedTags = tags.split(",").map((tag) => tag.trim()).filter(Boolean);
    if (parsedTags.length > maxTags) {
      setError(`A bookmark may have at most ${maxTags} tags; this one has ${parsedTags.length}.`);
      return;
    }
    const overlong = parsedTags.find((tag) => [...tag].length > maxTagLength);
    if (overlong !== undefined) {
      setError(`Tags must be at most ${maxTagLength} characters: “${[...overlong].slice(0, 20).join("")}…”`);
      return;
    }
    setPhase("saving");
    setError(undefined);
    try {
      const input = { ...draft, tags: parsedTags };
      const updated = await updateBookmark(bookmark.id, input);
      setDraft({
        title: updated.title,
        description: updated.description,
        tags: updated.tags,
        collection: updated.collection,
      });
      setTags(updated.tags.join(", "));
      onUpdated(updated);
    } catch (cause) {
      setError(errorMessage(cause, "These details could not be saved. Try again."));
    } finally {
      setPhase("idle");
    }
  };

  const remove = async () => {
    setPhase("deleting");
    setError(undefined);
    try {
      await deleteBookmark(bookmark.id);
      onDeleted(bookmark.id);
    } catch (cause) {
      setError(errorMessage(cause, "This bookmark could not be removed. Try again."));
      setConfirmingRemove(false);
      setPhase("idle");
    }
  };

  return (
    <article className="bookmark-card">
      <div className="bookmark-source">
        <span>{bookmark.community_name} · #{bookmark.channel_name}</span>
        <Link to={`/messages/${bookmark.message_id}`}>{dateTime.format(new Date(bookmark.source_created_at))}</Link>
      </div>
      <blockquote>
        <strong>{displayActor(bookmark)}</strong>
        <p>{bookmark.content || "Message has no text content."}</p>
      </blockquote>
      <div className="bookmark-fields">
        <label>
          Title
          <input
            value={draft.title}
            maxLength={200}
            placeholder="A useful name for this saved message"
            onChange={(event) => setDraft({ ...draft, title: event.target.value })}
          />
        </label>
        <label>
          Collection
          <input
            value={draft.collection}
            maxLength={100}
            placeholder="e.g. Deck making"
            onChange={(event) => setDraft({ ...draft, collection: event.target.value })}
          />
        </label>
        <label className="bookmark-description">
          Description
          <textarea
            value={draft.description}
            maxLength={4000}
            rows={3}
            placeholder="Why this matters, or what the conversation established"
            onChange={(event) => setDraft({ ...draft, description: event.target.value })}
          />
        </label>
        <label className="bookmark-tags">
          Tags <span>comma-separated · up to {maxTags}, {maxTagLength} characters each</span>
          <input
            value={tags}
            placeholder="glue, veneer, suppliers"
            onChange={(event) => setTags(event.target.value)}
          />
        </label>
      </div>
      {error && <div className="form-error" role="alert">{error}</div>}
      {confirmingRemove && (
        <p className="muted" role="alert">
          Removing deletes this bookmark and the title, description and tags on it, and cannot be
          undone. The archived message itself is untouched.
        </p>
      )}
      <div className="bookmark-controls">
        <button
          type="button"
          className="primary-button"
          disabled={phase !== "idle" || confirmingRemove}
          onClick={() => void save()}
        >
          {phase === "saving" ? "Saving…" : "Save details"}
        </button>
        {confirmingRemove ? (
          <>
            <button
              ref={keepRef}
              type="button"
              className="secondary-button"
              disabled={phase !== "idle"}
              onClick={() => setConfirmingRemove(false)}
            >
              Keep bookmark
            </button>
            <button
              type="button"
              className="danger-button"
              disabled={phase !== "idle"}
              onClick={() => void remove()}
            >
              {phase === "deleting" ? "Removing…" : "Remove permanently"}
            </button>
          </>
        ) : (
          <button
            type="button"
            className="danger-button"
            disabled={phase !== "idle"}
            onClick={() => setConfirmingRemove(true)}
          >
            Remove bookmark
          </button>
        )}
      </div>
    </article>
  );
}

export default function Bookmarks() {
  const [bookmarks, setBookmarks] = useState<Bookmark[]>([]);
  const [phase, setPhase] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState("");
  const [collection, setCollection] = useState("");
  const [tag, setTag] = useState("");

  useEffect(() => {
    let cancelled = false;
    setPhase("loading");
    fetchBookmarks()
      .then((rows) => {
        if (!cancelled) {
          setBookmarks(rows);
          setPhase("ready");
        }
      })
      .catch((cause: unknown) => {
        if (!cancelled) {
          setError(errorMessage(cause, "Bookmarks could not be loaded. Reload the page."));
          setPhase("error");
        }
      });
    return () => { cancelled = true; };
  }, []);

  const collections = useMemo(() => [...new Set(bookmarks.map((row) => row.collection).filter(Boolean))].sort(), [bookmarks]);
  const tags = useMemo(() => [...new Set(bookmarks.flatMap((row) => row.tags))].sort(), [bookmarks]);
  const visible = bookmarks.filter((row) =>
    (!collection || row.collection === collection) && (!tag || row.tags.includes(tag)),
  );

  return (
    <div className="page bookmarks-page">
      <header className="page-header archive-header">
        <div>
          <h1>Bookmarks</h1>
          <p className="page-intro">Messages you saved from the archive, kept with your own title, description and tags.</p>
        </div>
      </header>
      {phase === "error" && <div className="card error-card" role="alert">{error}</div>}
      {phase === "loading" && <div className="card muted">Loading bookmarks…</div>}
      {phase === "ready" && bookmarks.length > 0 && (
        <div className="bookmark-filters card">
          <label>Collection
            <select value={collection} onChange={(event) => setCollection(event.target.value)}>
              <option value="">All collections</option>
              {collections.map((name) => <option key={name}>{name}</option>)}
            </select>
          </label>
          <label>Tag
            <select value={tag} onChange={(event) => setTag(event.target.value)}>
              <option value="">All tags</option>
              {tags.map((name) => <option key={name}>{name}</option>)}
            </select>
          </label>
          <span className="muted">{visible.length} of {bookmarks.length}</span>
        </div>
      )}
      {phase === "ready" && bookmarks.length === 0 && (
        <div className="card muted">Save a message from the archive timeline, or use Discord’s “Archive message” action.</div>
      )}
      {phase === "ready" && bookmarks.length > 0 && visible.length === 0 && (
        <div className="card muted">No bookmarks match these filters.</div>
      )}
      <div className="bookmark-list">
        {visible.map((bookmark) => (
          <BookmarkCard
            key={bookmark.id}
            bookmark={bookmark}
            onUpdated={(updated) => setBookmarks((rows) => rows.map((row) => row.id === updated.id ? updated : row))}
            onDeleted={(id) => setBookmarks((rows) => rows.filter((row) => row.id !== id))}
          />
        ))}
      </div>
    </div>
  );
}
