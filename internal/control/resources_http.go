package control

import "net/http"

func (h *httpHandler) mountResources() {
	h.mux.HandleFunc("POST /api/v1/nodes/{id}/domain", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Domain string `json:"domain"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if err := h.store.ChangeNodeDomain(r.Context(), h.actor(r), r.PathValue("id"), body.Domain); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("POST /api/v1/tailnets/{id}/enabled", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if body.Enabled == nil {
			httpError(w, ErrInvalid)
			return
		}
		if err := h.store.SetTailnetEnabled(r.Context(), h.actor(r), r.PathValue("id"), *body.Enabled); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("DELETE /api/v1/tailnets/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := h.store.DeleteTailnet(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("POST /api/v1/nodes/{id}/enabled", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if body.Enabled == nil {
			httpError(w, ErrInvalid)
			return
		}
		if err := h.store.SetNodeEnabled(r.Context(), h.actor(r), r.PathValue("id"), *body.Enabled); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("DELETE /api/v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := h.store.DeleteNode(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}
