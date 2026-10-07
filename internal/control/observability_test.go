package control

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"tailscale.com/types/key"
)

func TestExpiryEventsReachBothOwnersDeduplicateAndResolve(t *testing.T) {
	s, admin, member, n, _, _ := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	keys := []identityKey{{NodePublic: key.NewNode().Public().String(), NotAfter: now.Add(10 * time.Second)}}
	policyTailnet(t, s, member.ID, "shared", keys, now)
	policyGrant(t, s, n.ID, "shared", "active", now)
	if _, err := s.BuildPolicy(t.Context(), n.ID, now); err != nil {
		t.Fatal(err)
	}
	run := func() {
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
		defer cancel()
		if err := s.RunIdentitySync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(20 * time.Second)
	run()
	run()
	for _, owner := range []Actor{admin, member} {
		items, err := s.Events(t.Context(), owner)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range items {
			if event.Kind == "authorization_expired" && event.ResolvedAt == 0 {
				count++
			}
		}
		want := 1
		if owner.Role == "admin" {
			want = 2
		}
		if count != want {
			t.Fatal("expiry notification missing, duplicated or mis-scoped", owner.Username, count, want)
		}
	}
	keys[0].NotAfter = now.Add(time.Hour)
	body, _ := json.Marshal(keys)
	if _, err := s.db.Exec("UPDATE identity_snapshots SET keys_json=?,last_success=? WHERE tailnet_id='shared'", body, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildPolicy(t.Context(), n.ID, now); err != nil {
		t.Fatal(err)
	}
	run()
	items, err := s.Events(t.Context(), member)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ResolvedAt != now.Unix() {
		t.Fatal("renewed identity failed to resolve expiry", items)
	}
}

func TestNodeBudgetReportDistinguishesMissingAndUnlimited(t *testing.T) {
	for _, input := range []string{`{"revision":1}`, `{"revision":1,"local_max_budget_bps":0,"effective_budget_bps":40000000}`} {
		var report cluster.NodeReport
		if err := json.Unmarshal([]byte(input), &report); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(report)
		var actual map[string]any
		json.Unmarshal(body, &actual)
		if strings.Contains(input, "local_max_budget_bps") {
			if value, ok := actual["local_max_budget_bps"]; !ok || value != float64(0) || actual["effective_budget_bps"] != float64(40000000) {
				t.Fatal("explicit unlimited/observed budget lost", string(body))
			}
		} else if _, ok := actual["local_max_budget_bps"]; ok {
			t.Fatal("old report inferred a limit")
		}
	}
}

func TestNodeRuntimeReportIsScopedTimedAndIndependentOfACK(t *testing.T) {
	s, admin, member, n, _, session := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	for _, item := range []struct{ owner, id string }{{admin.ID, "provider-private"}, {member.ID, "member-own"}} {
		policyTailnet(t, s, item.owner, item.id, []identityKey{{NodePublic: key.NewNode().Public().String(), NotAfter: now.Add(time.Hour)}}, now)
		policyGrant(t, s, n.ID, item.id, "active", now)
	}
	p, err := s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPHandler(s)
	report := map[string]any{"revision": p.Revision, "usable": true, "observed_at": now, "local_max_budget_bps": 40000000, "effective_budget_bps": 40000000, "active_connections": 42, "traffic": []map[string]any{
		{"tailnet_id": "provider-private", "rx_payload_bytes": 999},
		{"tailnet_id": "member-own", "rx_payload_bytes": 123, "tx_payload_bytes": 100, "relayed_payload_bytes": 100},
	}}
	post := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"report": report})
		r := httptest.NewRequest("POST", "/cluster/v1/heartbeat", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer "+session.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(); w.Code != 200 {
		t.Fatal("runtime report rejected", w.Code, w.Body.String())
	}
	cookie, csrf := loginTest(t, h, "alice")
	path := "/api/v1/nodes/" + n.ID + "/status"
	w := accountRequest(h, cookie, csrf, "GET", path, nil)
	var state struct {
		Applied uint64 `json:"applied_revision"`
		Report  struct {
			LocalMaxBudgetBPS  *uint64                  `json:"local_max_budget_bps"`
			EffectiveBudgetBPS *uint64                  `json:"effective_budget_bps"`
			Revision           uint64                   `json:"revision"`
			ObservedAt         time.Time                `json:"observed_at"`
			Traffic            []cluster.TailnetTraffic `json:"traffic"`
		} `json:"report"`
		ReportedAt int64 `json:"reported_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || state.Applied != 0 || state.Report.Revision != p.Revision || !state.Report.ObservedAt.Equal(now) || state.ReportedAt != now.Unix() || len(state.Report.Traffic) != 1 || state.Report.Traffic[0].RXPayloadBytes != 123 || strings.Contains(w.Body.String(), "provider-private") {
		t.Fatal("report source, privacy or ACK boundary failed", w.Body.String())
	}
	if state.Report.LocalMaxBudgetBPS == nil || *state.Report.LocalMaxBudgetBPS != 40000000 || state.Report.EffectiveBudgetBPS == nil || *state.Report.EffectiveBudgetBPS != 40000000 || strings.Contains(w.Body.String(), "active_connections") || strings.Contains(w.Body.String(), "instance_id") {
		t.Fatal("budget/revision scope failed", w.Body)
	}
	for _, change := range []func(){
		func() { report["revision"] = p.Revision + 1 },
		func() { report["revision"] = p.Revision; report["observed_at"] = now.Add(time.Minute) },
		func() { report["observed_at"] = now; report["traffic"] = []map[string]any{{"tailnet_id": "unrelated"}} },
	} {
		change()
		if w := post(); w.Code != 400 {
			t.Fatal("invalid report accepted", w.Code, w.Body.String())
		}
	}
	now = now.Add(time.Minute)
	w = accountRequest(h, cookie, csrf, "GET", path, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.ReportedAt != now.Add(-time.Minute).Unix() {
		t.Fatal("reading status refreshed report timestamp")
	}
}

func TestNodeBudgetReportPreservesScopeAndRevision(t *testing.T) {
	TestNodeRuntimeReportIsScopedTimedAndIndependentOfACK(t)
}

func TestEventsAndAuditOnlyExposeRelatedOwnerRecords(t *testing.T) {
	s, admin, member := nodeTestStore(t)
	for _, a := range []Actor{admin, member} {
		if _, err := s.db.Exec("INSERT INTO events(owner_id,resource_type,resource_id,kind,message,created_at) VALUES(?,'tailnet',?,'identity_unavailable','Identity source unavailable.',1)", a.ID, "resource-"+a.Username); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("INSERT INTO audit(actor_id,owner_id,resource_type,resource_id,action,created_at) VALUES(?,?,'tailnet',?,'credential.replace',1)", admin.ID, a.ID, "resource-"+a.Username); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "alice")
	for _, path := range []string{"/api/v1/events", "/api/v1/audit"} {
		w := accountRequest(h, cookie, csrf, "GET", path, nil)
		if w.Code != 200 {
			t.Fatal("observation API missing", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "resource-admin") || !strings.Contains(w.Body.String(), "resource-alice") {
			t.Fatal("owner record filtering failed", path, w.Body.String())
		}
	}
	adminCookie, adminCSRF := loginTest(t, h, "admin")
	w := accountRequest(h, adminCookie, adminCSRF, "GET", "/api/v1/audit", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "resource-admin") || !strings.Contains(w.Body.String(), "actor_username") {
		t.Fatal("administrator audit omitted actual actor", w.Body.String())
	}
}

func TestRelaySummaryCanBeReadBeforeApplyingWithoutPrivateTraffic(t *testing.T) {
	s, admin, _, n, _, _ := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	policyTailnet(t, s, admin.ID, "provider-private", []identityKey{{NodePublic: key.NewNode().Public().String(), NotAfter: now.Add(time.Hour)}}, now)
	policyGrant(t, s, n.ID, "provider-private", "active", now)
	if _, err := s.BuildPolicy(t.Context(), n.ID, now); err != nil {
		t.Fatal(err)
	}
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "alice")
	w := accountRequest(h, cookie, csrf, "GET", "/api/v1/nodes/"+n.ID+"/summary", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "budget_bps") || !strings.Contains(w.Body.String(), "provider") || strings.Contains(w.Body.String(), "provider-private") || strings.Contains(w.Body.String(), "traffic") || strings.Contains(w.Body.String(), "instance_id") {
		t.Fatal("directory summary missing or overexposed", w.Code, w.Body.String())
	}
}

func TestNodeTrafficRateUsesSuccessiveRuntimeSamples(t *testing.T) {
	s, admin, _, n, _, session := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	policyTailnet(t, s, admin.ID, "own", []identityKey{{NodePublic: key.NewNode().Public().String(), NotAfter: now.Add(time.Hour)}}, now)
	policyGrant(t, s, n.ID, "own", "active", now)
	p, err := s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	report := cluster.NodeReport{Revision: p.Revision, ObservedAt: now, Traffic: []cluster.TailnetTraffic{{CounterID: 1, TailnetID: "own", RXPayloadBytes: 100, TXPayloadBytes: 200}}}
	if _, err := s.NodeHeartbeat(t.Context(), session.Token, &report); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	report.Revision++
	report.ObservedAt = now
	report.Traffic[0].RXPayloadBytes += 1000
	report.Traffic[0].TXPayloadBytes += 2000
	if _, err := s.NodeHeartbeat(t.Context(), session.Token, &report); err != nil {
		t.Fatal(err)
	}
	state, err := s.NodeStatus(t.Context(), admin, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(state)
	var result struct {
		Rates []struct {
			RX      float64 `json:"rx_bits_per_second"`
			TX      float64 `json:"tx_bits_per_second"`
			Seconds float64 `json:"interval_seconds"`
		} `json:"traffic_rates"`
	}
	json.Unmarshal(body, &result)
	if len(result.Rates) != 1 || result.Rates[0].RX != 800 || result.Rates[0].TX != 1600 || result.Rates[0].Seconds != 10 {
		t.Fatal("rate not derived from actual observation interval", string(body))
	}
	now = now.Add(10 * time.Second)
	report.ObservedAt = now
	report.Traffic[0].CounterID++
	report.Traffic[0].RXPayloadBytes += 1000
	report.Traffic[0].TXPayloadBytes += 2000
	if _, err := s.NodeHeartbeat(t.Context(), session.Token, &report); err != nil {
		t.Fatal(err)
	}
	state, err = s.NodeStatus(t.Context(), admin, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(state)
	json.Unmarshal(body, &result)
	if len(result.Rates) != 0 {
		t.Fatal("rebuilt counter treated as continuous", string(body))
	}
	now = now.Add(10 * time.Second)
	report.ObservedAt = now
	report.Traffic[0].RXPayloadBytes = 1
	report.Traffic[0].TXPayloadBytes = 2
	if _, err := s.NodeHeartbeat(t.Context(), session.Token, &report); err != nil {
		t.Fatal(err)
	}
	state, err = s.NodeStatus(t.Context(), admin, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(state)
	json.Unmarshal(body, &result)
	if len(result.Rates) != 0 {
		t.Fatal("reset counter turned into measured throughput", string(body))
	}
}

func TestNodeStatusSeparatesReceivedAppliedAndExpiredPermission(t *testing.T) {
	s, admin, member, n, _, session := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	providerKey, memberKey := key.NewNode().Public().String(), key.NewNode().Public().String()
	policyTailnet(t, s, admin.ID, "provider-private", []identityKey{{NodePublic: providerKey, NotAfter: now.Add(30 * time.Second)}}, now)
	policyTailnet(t, s, member.ID, "member-own", []identityKey{{NodePublic: memberKey, NotAfter: now.Add(30 * time.Second)}}, now)
	policyGrant(t, s, n.ID, "provider-private", "active", now)
	policyGrant(t, s, n.ID, "member-own", "active", now)
	p, err := s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "alice")
	path := "/api/v1/nodes/" + n.ID + "/status"
	w := accountRequest(h, cookie, csrf, "GET", path, nil)
	if w.Code != 200 {
		t.Fatal("node status missing", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "provider-private") || strings.Contains(w.Body.String(), providerKey) || strings.Contains(w.Body.String(), memberKey) || strings.Contains(w.Body.String(), session.Token) {
		t.Fatal("status leaked another tailnet or authentication material")
	}
	var before struct {
		Desired        uint64 `json:"desired_revision"`
		Received       uint64 `json:"received_revision"`
		Applied        uint64 `json:"applied_revision"`
		ReportedUsable bool   `json:"reported_usable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.Desired != p.Revision || before.Applied != 0 || before.Received != 0 || before.ReportedUsable {
		t.Fatal("desired policy reported applied", before)
	}
	if err := s.AcknowledgePolicy(t.Context(), session.Token, cluster.PolicyACK{Revision: p.Revision, State: "received"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgePolicy(t.Context(), session.Token, cluster.PolicyACK{Revision: p.Revision, State: "applied", Usable: true}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	w = accountRequest(h, cookie, csrf, "GET", path, nil)
	var expired struct {
		Eligible       bool   `json:"policy_has_live_keys"`
		ReportedUsable bool   `json:"reported_usable"`
		Applied        uint64 `json:"applied_revision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &expired); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || expired.Eligible || !expired.ReportedUsable || expired.Applied != p.Revision {
		t.Fatal("expired permission confused with last ACK", w.Code, expired)
	}
}
