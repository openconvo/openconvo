package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/openconvo/openconvo/internal/backups"
)

// backupWriteDeadline is how long a backup download may take to stream. It
// replaces the server's WriteTimeout for this one route.
const backupWriteDeadline = 30 * time.Minute

func requireBackups(deps Deps, w http.ResponseWriter) bool {
	if deps.Backups == nil {
		writeError(w, http.StatusServiceUnavailable, "database backups are not available")
		return false
	}
	return true
}

func handleBackupSettings(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireBackups(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		settings, err := deps.Backups.GetSettings(r.Context())
		if err != nil {
			deps.Logger.Error("load database backup settings", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load backup settings")
			return
		}
		writeJSON(w, http.StatusOK, settings)
	}
}

func handleSaveBackupSettings(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireBackups(deps, w) {
			return
		}
		var input backups.Settings
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid backup settings")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			writeError(w, http.StatusBadRequest, "body must contain one JSON object")
			return
		}
		settings, err := deps.Backups.SaveSettings(r.Context(), input)
		if err != nil {
			if errors.Is(err, backups.ErrInvalidSettings) || errors.Is(err, backups.ErrNotConfigured) || errors.Is(err, backups.ErrDestination) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			deps.Logger.Error("save database backup settings", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to save backup settings")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, settings)
	}
}

func handleListBackups(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireBackups(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		items, err := deps.Backups.ListBackups(r.Context(), 50)
		if err != nil {
			deps.Logger.Error("list database backups", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list database backups")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"backups": items})
	}
}

func handleCreateBackup(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireBackups(deps, w) {
			return
		}
		backup, created, err := deps.Backups.RequestBackup(r.Context(), "manual")
		if err != nil {
			if errors.Is(err, backups.ErrNotConfigured) {
				writeError(w, http.StatusBadRequest, "configure a backup destination and environment credentials first")
				return
			}
			deps.Logger.Error("request database backup", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to request database backup")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusAccepted, map[string]any{"backup": backup, "created": created})
	}
}

func handleBackupContent(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireBackups(deps, w) {
			return
		}
		id := r.PathValue("id")
		if !uuidPattern.MatchString(id) {
			writeError(w, http.StatusNotFound, "database backup not found")
			return
		}
		backup, body, err := deps.Backups.OpenBackup(r.Context(), id)
		if errors.Is(err, backups.ErrNotFound) {
			writeError(w, http.StatusNotFound, "database backup not found")
			return
		}
		if err != nil {
			deps.Logger.Error("open database backup", "backup_id", id, "error", err)
			writeError(w, http.StatusBadGateway, "failed to open database backup")
			return
		}
		defer body.Close()

		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": backup.Filename()}))
		w.Header().Set("Content-Length", strconv.FormatInt(backup.Size, 10))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		// A dump of a real archive takes longer to send than the server's
		// WriteTimeout allows, so this download gets its own deadline. If the
		// writer cannot carry one, say so: the alternative is a large download
		// that is cut off at WriteTimeout for no visible reason.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(backupWriteDeadline)); err != nil {
			deps.Logger.Warn("could not extend the write deadline for a database backup download",
				"backup_id", id, "error", err)
		}
		if _, err := io.Copy(w, body); err != nil {
			deps.Logger.Warn("stream database backup", "backup_id", id, "error", fmt.Errorf("stream: %w", err))
		}
	}
}
