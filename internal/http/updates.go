package http

import "net/http"

func handleUpdate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		if deps.Updates == nil {
			writeError(w, http.StatusServiceUnavailable, "update check not available")
			return
		}
		status, err := deps.Updates.Check(r.Context())
		if err != nil {
			deps.Logger.Warn("check for updates", "error", err)
			writeError(w, http.StatusBadGateway, "could not check the latest release")
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}
