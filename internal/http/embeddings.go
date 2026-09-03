package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/openconvo/openconvo/internal/embeddings"
)

func requireEmbeddings(deps Deps, w http.ResponseWriter) bool {
	if deps.Embeddings == nil {
		writeError(w, http.StatusServiceUnavailable, "message embeddings are not available")
		return false
	}
	return true
}

func handleEmbeddingSettings(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireEmbeddings(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		settings, err := deps.Embeddings.GetSettings(r.Context())
		if err != nil {
			deps.Logger.Error("load embedding settings", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load embedding settings")
			return
		}
		writeJSON(w, http.StatusOK, settings)
	}
}

func handleSaveEmbeddingSettings(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireEmbeddings(deps, w) {
			return
		}
		var input embeddings.Settings
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid embedding settings")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			writeError(w, http.StatusBadRequest, "body must contain one JSON object")
			return
		}
		settings, err := deps.Embeddings.SaveSettings(r.Context(), input)
		if err != nil {
			if errors.Is(err, embeddings.ErrInvalidSettings) || errors.Is(err, embeddings.ErrNotConfigured) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			deps.Logger.Error("save embedding settings", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to save embedding settings")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, settings)
	}
}
