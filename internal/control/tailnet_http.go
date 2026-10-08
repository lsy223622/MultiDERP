package control

import "net/http"

func (h *httpHandler) mountTailnets() {
	s := h.store
	h.mux.HandleFunc("GET /api/v1/tailnets", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.ListTailnets(r.Context(), h.actor(r))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, items)
	})
	h.mux.HandleFunc("GET /api/v1/tailnets/{id}", func(w http.ResponseWriter, r *http.Request) {
		item, err := s.Tailnet(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, item)
	})
	h.mux.HandleFunc("POST /api/v1/tailnets", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DisplayName string          `json:"display_name"`
			APIID       string          `json:"api_id"`
			Credential  OAuthCredential `json:"credential"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		item, err := s.AddTailnet(r.Context(), h.actor(r), body.DisplayName, body.APIID, body.Credential)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, item)
	})
	h.mux.HandleFunc("POST /api/v1/tailnets/{id}/credential", func(w http.ResponseWriter, r *http.Request) {
		actor := h.actor(r)
		id := r.PathValue("id")
		if _, err := s.Tailnet(r.Context(), actor, id); err != nil {
			httpError(w, err)
			return
		}
		var c OAuthCredential
		if !decodeRequest(w, r, &c) {
			return
		}
		if err := s.ReplaceCredential(r.Context(), actor, id, c); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("DELETE /api/v1/tailnets/{id}/credential", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DeleteCredential(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("POST /api/v1/tailnets/{id}/transfer", func(w http.ResponseWriter, r *http.Request) {
		actor := h.actor(r)
		if actor.Role != "admin" {
			httpError(w, ErrForbidden)
			return
		}
		var body struct {
			OwnerID string `json:"owner_id"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if err := s.TransferTailnet(r.Context(), actor, r.PathValue("id"), body.OwnerID); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}
