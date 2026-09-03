package http

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/storage"
)

// attachmentWriteDeadline is how long a single attachment download may take to
// stream. It replaces the server's WriteTimeout for this one route.
const attachmentWriteDeadline = 10 * time.Minute

func handleAttachmentContent(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		if deps.Blobs == nil {
			writeError(w, http.StatusServiceUnavailable, "attachment storage not available")
			return
		}
		id := r.PathValue("id")
		if !uuidPattern.MatchString(id) {
			writeError(w, http.StatusNotFound, "attachment not found")
			return
		}
		attachment, ok, err := deps.Archive.GetStoredAttachment(r.Context(), id)
		if err != nil {
			deps.Logger.Error("load stored attachment", "attachment_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load attachment")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "attachment not found")
			return
		}

		if r.Method == http.MethodHead {
			setAttachmentHeaders(w, attachment)
			w.WriteHeader(http.StatusOK)
			return
		}

		body, err := deps.Blobs.Open(r.Context(), attachment.SHA256)
		if errors.Is(err, storage.ErrNotFound) {
			deps.Logger.Error("stored attachment object is missing", "attachment_id", id)
			writeError(w, http.StatusServiceUnavailable, "attachment object is unavailable")
			return
		}
		if err != nil {
			deps.Logger.Error("open attachment object", "attachment_id", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "attachment object is unavailable")
			return
		}
		defer body.Close()

		setAttachmentHeaders(w, attachment)
		w.WriteHeader(http.StatusOK)
		// An attachment may be up to attachments.DefaultMaxBytes, which on a
		// slow link takes longer to send than the server's WriteTimeout
		// allows, so this stream gets its own deadline.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(attachmentWriteDeadline)); err != nil {
			deps.Logger.Warn("could not extend the write deadline for an attachment download",
				"attachment_id", id, "error", err)
		}
		if _, err := io.Copy(w, body); err != nil && r.Context().Err() == nil {
			deps.Logger.Warn("stream attachment", "attachment_id", id, "error", err)
		}
	}
}

func setAttachmentHeaders(w http.ResponseWriter, attachment archive.StoredAttachment) {
	contentType := "application/octet-stream"
	if mediaType, _, err := mime.ParseMediaType(attachment.ContentType); err == nil && mediaType != "" {
		contentType = mediaType
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{
		"filename": safeDownloadFilename(attachment.Filename),
	})
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.FormatInt(attachment.Size, 10))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func safeDownloadFilename(filename string) string {
	filename = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return -1
		case r == '/' || r == '\\':
			return '_'
		default:
			return r
		}
	}, strings.TrimSpace(filename))
	runes := []rune(filename)
	if len(runes) > 180 {
		filename = string(runes[:180])
	}
	if filename == "" || filename == "." || filename == ".." {
		return "attachment"
	}
	return filename
}
