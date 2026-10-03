package control

import (
	"context"
	"net/http"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func (s *Store) ListRelays(ctx context.Context, actor Actor) ([]Node, error) {
	if err := RequireOwner(actor, actor.ID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT n.id,n.owner_id,n.domain,n.display_name,n.region_id,n.state,n.last_heartbeat,n.last_error,n.enabled FROM nodes n JOIN users u ON u.id=n.owner_id WHERE u.enabled=1 AND n.enabled=1 AND n.state IN ('registered','ready','offline') ORDER BY n.display_name,n.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (h *httpHandler) mountGrants() {
	s := h.store
	h.mux.HandleFunc("GET /api/v1/relays", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.ListRelays(r.Context(), h.actor(r))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, items)
	})
	h.mux.HandleFunc("GET /api/v1/grants", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.ListGrants(r.Context(), h.actor(r))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, items)
	})
	h.mux.HandleFunc("GET /api/v1/grants/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, err := s.Grant(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, g)
	})
	h.mux.HandleFunc("POST /api/v1/grants", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NodeID           string    `json:"node_id"`
			TailnetID        string    `json:"tailnet_id"`
			ExpectedRevision uint64    `json:"expected_revision"`
			ExplicitUntil    time.Time `json:"explicit_until"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		g, err := s.RequestGrant(r.Context(), h.actor(r), body.NodeID, body.TailnetID, body.ExpectedRevision, body.ExplicitUntil)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, g)
	})
	h.mux.HandleFunc("POST /api/v1/grants/{id}/actions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ExpectedRevision uint64      `json:"expected_revision"`
			Action           GrantAction `json:"action"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		g, err := s.ApplyGrantAction(r.Context(), h.actor(r), r.PathValue("id"), body.ExpectedRevision, body.Action)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, g)
	})
	h.mux.HandleFunc("GET /api/v1/tailnets/{id}/derpmap", func(w http.ResponseWriter, r *http.Request) {
		m, err := s.BuildDERPMap(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, m)
	})
	h.mux.HandleFunc("GET /api/v1/nodes/{id}/qos", func(w http.ResponseWriter, r *http.Request) {
		q, err := s.NodeQoS(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, q)
	})
	h.mux.HandleFunc("POST /api/v1/nodes/{id}/qos", func(w http.ResponseWriter, r *http.Request) {
		var q cluster.QoSPolicy
		if !decodeRequest(w, r, &q) {
			return
		}
		if err := s.SetNodeQoS(r.Context(), h.actor(r), r.PathValue("id"), q); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}
