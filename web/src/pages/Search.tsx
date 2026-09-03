import { type FormEvent, type ReactNode, useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  fetchArchiveChannels,
  searchMessages,
  type ArchiveChannel,
  type SearchOptions,
  type SearchResult,
} from "../api";
import { errorMessage } from "./errorMessage";

const pageSize = 25;

type ResultsState =
  | { phase: "idle" }
  | { phase: "loading" }
  | { phase: "error"; message: string }
  | { phase: "ready"; results: SearchResult[]; hasMore: boolean };

function localDateBoundary(value: string | null): string | undefined {
  if (!value) return undefined;
  const parts = value.split("-").map(Number);
  if (parts.length !== 3 || parts.some((part) => !Number.isInteger(part))) return undefined;
  const boundary = new Date(parts[0], parts[1] - 1, parts[2]);
  // A shared or hand-edited link can carry anything here, and toISOString
  // throws on an unrepresentable date — which would take the page down.
  if (Number.isNaN(boundary.getTime())) return undefined;
  return boundary.toISOString();
}

// Date bounds that could not be read are dropped rather than sent, so the
// page has to say it searched a wider range than the link asked for.
function unreadableDateFilters(params: URLSearchParams): string[] {
  return (["after", "before"] as const)
    .filter((name) => params.get(name) && localDateBoundary(params.get(name)) === undefined)
    .map((name) => (name === "after" ? "After" : "Before"));
}

function optionsFromParams(params: URLSearchParams): SearchOptions | null {
  const query = params.get("q")?.trim() ?? "";
  if (!query) return null;
  return {
    query,
    mode: params.get("mode") === "semantic" ? "semantic" : "keyword",
    channelId: params.get("channel") || undefined,
    author: params.get("author") || undefined,
    after: localDateBoundary(params.get("after")),
    before: localDateBoundary(params.get("before")),
    hasAttachment: params.get("attachments") === "true",
    limit: pageSize,
  };
}

function actorName(result: SearchResult): string {
  return result.actor?.display_name || result.actor?.username || "Unknown author";
}

// Keyword search inserts only these two delimiters; semantic excerpts contain
// neither. Rendering every other piece as React text keeps archived message
// content inert even when it contains HTML.
function HighlightedExcerpt({ excerpt }: { excerpt: string }) {
  const nodes: ReactNode[] = [];
  let marked = false;
  excerpt.split(/(<mark>|<\/mark>)/).forEach((part, index) => {
    if (part === "<mark>") {
      marked = true;
    } else if (part === "</mark>") {
      marked = false;
    } else if (part) {
      nodes.push(marked ? <mark key={index}>{part}</mark> : <span key={index}>{part}</span>);
    }
  });
  return <>{nodes}</>;
}

export default function Search() {
  const [params, setParams] = useSearchParams();
  const [query, setQuery] = useState(params.get("q") ?? "");
  const [mode, setMode] = useState<"keyword" | "semantic">(
    params.get("mode") === "semantic" ? "semantic" : "keyword",
  );
  const [channel, setChannel] = useState(params.get("channel") ?? "");
  const [author, setAuthor] = useState(params.get("author") ?? "");
  const [after, setAfter] = useState(params.get("after") ?? "");
  const [before, setBefore] = useState(params.get("before") ?? "");
  const [hasAttachment, setHasAttachment] = useState(params.get("attachments") === "true");
  const [channels, setChannels] = useState<ArchiveChannel[]>([]);
  const [results, setResults] = useState<ResultsState>({ phase: "idle" });
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadMoreError, setLoadMoreError] = useState("");
  const [filterError, setFilterError] = useState("");

  useEffect(() => {
    let cancelled = false;
    fetchArchiveChannels()
      .then((loaded) => {
        if (!cancelled) setChannels(loaded);
      })
      .catch(() => {
        // Search remains usable without the optional channel picker.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    setQuery(params.get("q") ?? "");
    setMode(params.get("mode") === "semantic" ? "semantic" : "keyword");
    setChannel(params.get("channel") ?? "");
    setAuthor(params.get("author") ?? "");
    setAfter(params.get("after") ?? "");
    setBefore(params.get("before") ?? "");
    setHasAttachment(params.get("attachments") === "true");
    setFilterError("");
    setLoadMoreError("");

    const options = optionsFromParams(params);
    if (!options) {
      setResults({ phase: "idle" });
      return;
    }
    let cancelled = false;
    setResults({ phase: "loading" });
    searchMessages(options)
      .then((page) => {
        if (!cancelled) {
          setResults({ phase: "ready", results: page.results, hasMore: page.has_more });
        }
      })
      .catch((error: unknown) => {
        if (!cancelled) {
          setResults({
            phase: "error",
            message: errorMessage(error, "The search could not be completed. Try again."),
          });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [params]);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    // The API rejects a range that is not strictly increasing; saying so here
    // beats sending the search and reporting a rejection.
    if (after && before && after >= before) {
      setFilterError("After must be an earlier date than Before.");
      return;
    }
    setFilterError("");
    const next = new URLSearchParams();
    next.set("q", query.trim());
    if (mode === "semantic") next.set("mode", "semantic");
    if (channel) next.set("channel", channel);
    if (author.trim()) next.set("author", author.trim());
    if (after) next.set("after", after);
    if (before) next.set("before", before);
    if (hasAttachment) next.set("attachments", "true");
    setParams(next);
  }

  function clearFilters() {
    setFilterError("");
    setChannel("");
    setAuthor("");
    setAfter("");
    setBefore("");
    setHasAttachment(false);
  }

  function loadMore() {
    const options = optionsFromParams(params);
    if (!options || results.phase !== "ready") return;
    const loaded = results.results;
    setLoadingMore(true);
    setLoadMoreError("");
    searchMessages({ ...options, offset: loaded.length })
      .then((page) => {
        setResults({
          phase: "ready",
          results: [...loaded, ...page.results],
          hasMore: page.has_more,
        });
      })
      .catch((error: unknown) => {
        // Report the failed page beside the results already on screen; losing
        // them would send the operator back to the first page of the search.
        setLoadMoreError(errorMessage(error, "Could not load more results. Try again."));
      })
      .finally(() => setLoadingMore(false));
  }

  const unreadableDates = unreadableDateFilters(params);

  return (
    <div className="page search-page">
      <header className="page-header archive-header">
        <div>
          <h1>Search</h1>
          <p className="page-intro">Search archived messages and open any result in context.</p>
        </div>
      </header>

      <form className="search-form card" onSubmit={submit}>
        <div className="search-query-row">
          <label htmlFor="search-query">Message text</label>
          <div>
            <input
              id="search-query"
              type="search"
              required
              maxLength={500}
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder={mode === "semantic" ? "Try advice for bonding wood" : 'Try maple veneer or "maple veneer"'}
            />
            <button type="submit">Search</button>
          </div>
        </div>
        <fieldset className="search-mode">
          <legend>Search method</legend>
          <label>
            <input
              type="radio"
              name="search-mode"
              value="keyword"
              checked={mode === "keyword"}
              onChange={() => setMode("keyword")}
            />
            Keyword
          </label>
          <label>
            <input
              type="radio"
              name="search-mode"
              value="semantic"
              checked={mode === "semantic"}
              onChange={() => setMode("semantic")}
            />
            Semantic
          </label>
          <span>{mode === "semantic" ? "Sends this search query to OpenAI." : "Uses local PostgreSQL full-text search."}</span>
        </fieldset>
        <div className="search-filters">
          <label>
            Channel
            <select value={channel} onChange={(event) => setChannel(event.target.value)}>
              <option value="">All channels</option>
              {channels.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.community_name} · #{item.name || "unnamed-channel"}
                </option>
              ))}
            </select>
          </label>
          <label>
            Author
            <input
              value={author}
              maxLength={200}
              onChange={(event) => setAuthor(event.target.value)}
              placeholder="Username or display name"
            />
          </label>
          <label>
            After
            <input type="date" value={after} onChange={(event) => setAfter(event.target.value)} />
          </label>
          <label>
            Before
            <input type="date" value={before} onChange={(event) => setBefore(event.target.value)} />
          </label>
          <label className="search-checkbox">
            <input
              type="checkbox"
              checked={hasAttachment}
              onChange={(event) => setHasAttachment(event.target.checked)}
            />
            Has attachment
          </label>
          <button type="button" className="filter-clear" onClick={clearFilters}>
            Clear filters
          </button>
        </div>
        {filterError && (
          <div className="form-error" role="alert">
            {filterError}
          </div>
        )}
      </form>

      {unreadableDates.length > 0 && (
        <div className="form-error" role="alert">
          {unreadableDates.length > 1
            ? `The ${unreadableDates.join(" and ")} filters in this link are not dates ` +
              "(YYYY-MM-DD), so both were ignored."
            : `The ${unreadableDates[0]} filter in this link is not a date ` +
              "(YYYY-MM-DD), so it was ignored."}
        </div>
      )}

      {results.phase === "idle" && (
        <div className="card muted">
          {mode === "semantic"
            ? "Describe the meaning you want to find."
            : "Enter a word or quoted phrase to search the archive."}
        </div>
      )}
      {results.phase === "loading" && (
        <div className="card muted">
          {params.get("mode") === "semantic" ? "Searching semantically…" : "Searching the archive…"}
        </div>
      )}
      {results.phase === "error" && (
        <div className="card error-card" role="alert">
          {results.message}
        </div>
      )}
      {results.phase === "ready" && results.results.length === 0 && (
        <div className="card muted">No archived messages matched this search.</div>
      )}
      {results.phase === "ready" && results.results.length > 0 && (
        <>
          <p className="search-summary">
            {results.results.length.toLocaleString()} {results.hasMore ? "or more " : ""}results
          </p>
          <div className="search-results">
            {results.results.map((result) => (
              <Link key={result.message_id} className="search-result" to={`/messages/${result.message_id}`}>
                <div className="search-result-heading">
                  <strong>{actorName(result)}</strong>
                  <span>{result.community_name} · #{result.channel_name}</span>
                  <time dateTime={result.source_created_at}>
                    {new Date(result.source_created_at).toLocaleString()}
                  </time>
                </div>
                <p><HighlightedExcerpt excerpt={result.excerpt} /></p>
                <div className="search-result-footer">
                  {result.has_attachment && <span>Attachment</span>}
                  <span>Open in context →</span>
                </div>
              </Link>
            ))}
          </div>
          {loadMoreError && (
            <div className="form-error" role="alert">
              {loadMoreError}
            </div>
          )}
          {results.hasMore && (
            <div className="older-control">
              <button type="button" disabled={loadingMore} onClick={loadMore}>
                {loadingMore ? "Loading…" : "Load more results"}
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
