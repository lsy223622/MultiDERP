package control

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

type GrantAction string

const (
	GrantApprove GrantAction = "approve"
	GrantConfirm GrantAction = "confirm"
	GrantCancel  GrantAction = "cancel"
	GrantReject  GrantAction = "reject"
	GrantRevoke  GrantAction = "revoke"
	GrantLeave   GrantAction = "leave"
)

type Grant struct {
	ID             string    `json:"id"`
	NodeID         string    `json:"node_id"`
	TailnetID      string    `json:"tailnet_id"`
	NodeOwnerID    string    `json:"node_owner_id"`
	TailnetOwnerID string    `json:"tailnet_owner_id"`
	State          string    `json:"state"`
	Revision       uint64    `json:"revision"`
	ExplicitUntil  time.Time `json:"explicit_until"`
	NodeName       string    `json:"node_name"`
	TailnetName    string    `json:"tailnet_name"`
	Applicant      string    `json:"applicant"`
}

const grantColumns = `g.id,g.node_id,g.tailnet_id,n.owner_id,t.owner_id,g.state,g.revision,g.explicit_until,n.display_name,t.display_name,(SELECT username FROM users WHERE id=t.owner_id)`
const grantJoins = ` FROM grants g JOIN nodes n ON n.id=g.node_id JOIN tailnets t ON t.id=g.tailnet_id`

func scanGrant(row interface{ Scan(...any) error }, now time.Time) (Grant, error) {
	var g Grant
	var until int64
	err := row.Scan(&g.ID, &g.NodeID, &g.TailnetID, &g.NodeOwnerID, &g.TailnetOwnerID, &g.State, &g.Revision, &until, &g.NodeName, &g.TailnetName, &g.Applicant)
	if until != 0 {
		g.ExplicitUntil = time.Unix(until, 0).UTC()
		if !now.Before(g.ExplicitUntil) && (g.State == "requested" || g.State == "owner_approved" || g.State == "active") {
			g.State = "expired"
		}
	}
	return g, err
}

func (s *Store) Grant(ctx context.Context, actor Actor, id string) (Grant, error) {
	g, err := scanGrant(s.db.QueryRowContext(ctx, "SELECT "+grantColumns+grantJoins+" WHERE g.id=?", id), s.now())
	if err != nil {
		return Grant{}, err
	}
	if RequireOwner(actor, g.NodeOwnerID) != nil && RequireOwner(actor, g.TailnetOwnerID) != nil {
		return Grant{}, ErrForbidden
	}
	return g, nil
}

func (s *Store) ListGrants(ctx context.Context, actor Actor) ([]Grant, error) {
	if err := RequireOwner(actor, actor.ID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+grantColumns+grantJoins+" WHERE n.owner_id=? OR t.owner_id=? OR ?='admin' ORDER BY g.id", actor.ID, actor.ID, actor.Role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Grant{}
	for rows.Next() {
		g, err := scanGrant(rows, s.now())
		if err != nil {
			return nil, err
		}
		items = append(items, g)
	}
	return items, rows.Err()
}

func grantResourcesEnabled(ctx context.Context, tx *sql.Tx, node, tailnet string) (bool, error) {
	var enabled bool
	err := tx.QueryRowContext(ctx, `SELECT n.enabled=1 AND n.state IN ('registered','ready','offline') AND nu.enabled=1 AND t.enabled=1 AND tu.enabled=1 FROM nodes n JOIN users nu ON nu.id=n.owner_id JOIN tailnets t ON t.id=? JOIN users tu ON tu.id=t.owner_id WHERE n.id=?`, tailnet, node).Scan(&enabled)
	return enabled, err
}

func (s *Store) RequestGrant(ctx context.Context, actor Actor, node, tailnet string, expected uint64, until time.Time) (Grant, error) {
	if expected >= math.MaxInt64 || (!until.IsZero() && !s.now().Before(until.Truncate(time.Second))) {
		return Grant{}, ErrInvalid
	}
	var untilUnix int64
	if !until.IsZero() {
		until = until.UTC().Truncate(time.Second)
		untilUnix = until.Unix()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return Grant{}, err
	}
	var nodeOwner, tailnetOwner, nodeName, tailnetName, applicant string
	if err := tx.QueryRowContext(ctx, "SELECT n.owner_id,t.owner_id,n.display_name,t.display_name,(SELECT username FROM users WHERE id=t.owner_id) FROM nodes n JOIN tailnets t ON t.id=? WHERE n.id=?", tailnet, node).Scan(&nodeOwner, &tailnetOwner, &nodeName, &tailnetName, &applicant); err != nil {
		return Grant{}, err
	}
	if err := RequireOwner(actor, tailnetOwner); err != nil {
		return Grant{}, err
	}
	enabled, err := grantResourcesEnabled(ctx, tx, node, tailnet)
	if err != nil {
		return Grant{}, err
	}
	if !enabled {
		return Grant{}, ErrConflict
	}
	state := "requested"
	if nodeOwner == tailnetOwner {
		state = "active"
	}
	g, err := scanGrant(tx.QueryRowContext(ctx, "SELECT "+grantColumns+grantJoins+" WHERE g.node_id=? AND g.tailnet_id=?", node, tailnet), s.now())
	if errors.Is(err, sql.ErrNoRows) {
		if expected != 0 {
			return Grant{}, ErrConflict
		}
		g = Grant{ID: randomToken(), NodeID: node, TailnetID: tailnet, NodeOwnerID: nodeOwner, TailnetOwnerID: tailnetOwner, NodeName: nodeName, TailnetName: tailnetName, Applicant: applicant}
	} else if err != nil {
		return Grant{}, err
	} else {
		if g.State == state && g.ExplicitUntil.Equal(until) && (expected == g.Revision || expected+1 == g.Revision) {
			return g, nil
		}
		if expected != g.Revision || (g.State != "cancelled" && g.State != "rejected" && g.State != "revoked" && g.State != "expired") {
			return Grant{}, ErrConflict
		}
	}
	g.State, g.Revision, g.ExplicitUntil = state, g.Revision+1, until
	var controlSuccess int64
	if state == "active" {
		controlSuccess = s.now().Unix()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO grants(id,node_id,tailnet_id,state,revision,explicit_until,last_control_success) VALUES(?,?,?,?,?,?,?) ON CONFLICT(node_id,tailnet_id) DO UPDATE SET state=excluded.state,revision=excluded.revision,explicit_until=excluded.explicit_until,last_control_success=excluded.last_control_success`, g.ID, node, tailnet, state, g.Revision, untilUnix, controlSuccess); err != nil {
		return Grant{}, err
	}
	if _, err := s.buildPolicy(ctx, tx, node, s.now()); err != nil {
		return Grant{}, err
	}
	if err := writeAudit(ctx, tx, actor.ID, tailnetOwner, "grant", g.ID, "grant.request"); err != nil {
		return Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return Grant{}, err
	}
	s.notifyPolicy(node)
	return g, nil
}

func (s *Store) ApplyGrantAction(ctx context.Context, actor Actor, id string, expected uint64, action GrantAction) (Grant, error) {
	if expected == 0 || expected >= math.MaxInt64 {
		return Grant{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return Grant{}, err
	}
	g, err := scanGrant(tx.QueryRowContext(ctx, "SELECT "+grantColumns+grantJoins+" WHERE g.id=?", id), s.now())
	if err != nil {
		return Grant{}, err
	}
	var owner, next string
	var allowed bool
	switch action {
	case GrantApprove:
		owner, next, allowed = g.NodeOwnerID, "owner_approved", g.State == "requested"
	case GrantConfirm:
		owner, next, allowed = g.TailnetOwnerID, "active", g.State == "owner_approved"
	case GrantCancel:
		owner, next, allowed = g.TailnetOwnerID, "cancelled", g.State == "requested" || g.State == "owner_approved"
	case GrantReject:
		owner, next, allowed = g.NodeOwnerID, "rejected", g.State == "requested"
	case GrantRevoke:
		owner, next, allowed = g.NodeOwnerID, "revoked", g.State == "owner_approved" || g.State == "active"
	case GrantLeave:
		owner, next, allowed = g.TailnetOwnerID, "revoked", g.State == "owner_approved" || g.State == "active"
	default:
		return Grant{}, ErrInvalid
	}
	if err := RequireOwner(actor, owner); err != nil {
		return Grant{}, err
	}
	if g.State == next && (g.Revision == expected || g.Revision == expected+1) {
		return g, nil
	}
	if g.Revision != expected || !allowed {
		return Grant{}, ErrConflict
	}
	if action == GrantApprove || action == GrantConfirm {
		enabled, err := grantResourcesEnabled(ctx, tx, g.NodeID, g.TailnetID)
		if err != nil {
			return Grant{}, err
		}
		if !enabled {
			return Grant{}, ErrConflict
		}
	}
	var controlSuccess int64
	if next == "active" {
		controlSuccess = s.now().Unix()
	}
	g.State, g.Revision = next, g.Revision+1
	if _, err := tx.ExecContext(ctx, "UPDATE grants SET state=?,revision=?,last_control_success=? WHERE id=?", next, g.Revision, controlSuccess, id); err != nil {
		return Grant{}, err
	}
	if _, err := s.buildPolicy(ctx, tx, g.NodeID, s.now()); err != nil {
		return Grant{}, err
	}
	if err := writeAudit(ctx, tx, actor.ID, owner, "grant", id, "grant."+string(action)); err != nil {
		return Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return Grant{}, err
	}
	s.notifyPolicy(g.NodeID)
	return g, nil
}
