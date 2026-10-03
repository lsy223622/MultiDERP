package control

import (
	"bytes"
	"context"
	"strconv"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"tailscale.com/tailcfg"
)

func (s *Store) BuildDERPMap(ctx context.Context, actor Actor, tailnet string) (*tailcfg.DERPMap, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tailnetOwner(ctx, tx, actor, tailnet); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT n.id,n.domain,n.display_name,n.region_id,p.policy_json FROM grants g JOIN nodes n ON n.id=g.node_id JOIN users nu ON nu.id=n.owner_id JOIN tailnets t ON t.id=g.tailnet_id JOIN users tu ON tu.id=t.owner_id JOIN node_policies p ON p.node_id=n.id WHERE g.tailnet_id=? AND g.state='active' AND n.enabled=1 AND n.state IN ('registered','ready','offline') AND nu.enabled=1 AND t.enabled=1 AND tu.enabled=1 ORDER BY n.region_id`, tailnet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := &tailcfg.DERPMap{Regions: make(map[int]*tailcfg.DERPRegion)}
	for rows.Next() {
		var id, domain, name string
		var region int
		var body []byte
		if err := rows.Scan(&id, &domain, &name, &region, &body); err != nil {
			return nil, err
		}
		p, err := cluster.DecodePolicy(bytes.NewReader(body), s.clusterID, id)
		if err != nil {
			return nil, err
		}
		usable := false
		for _, g := range p.Grants {
			if g.TailnetID != tailnet {
				continue
			}
			for _, k := range g.Keys {
				if s.now().Before(cluster.EffectiveUntil(g, k)) {
					usable = true
					break
				}
			}
		}
		if !usable {
			continue
		}
		if region < 900 || region > 999 || m.Regions[region] != nil {
			return nil, ErrConflict
		}
		code := "uniderp-" + strconv.Itoa(region)
		m.Regions[region] = &tailcfg.DERPRegion{RegionID: region, RegionCode: code, RegionName: name, Nodes: []*tailcfg.DERPNode{{Name: code + "a", RegionID: region, HostName: domain, DERPPort: 443, STUNPort: 3478}}}
	}
	return m, rows.Err()
}
