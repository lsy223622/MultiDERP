package control

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func identityFixture(k key.NodePublic) string {
	return fmt.Sprintf(`{"nodeKey":%q,"authorized":true,"isExternal":false,"keyExpiryDisabled":false,"expires":"2026-10-04T00:00:00Z"}`, k.String())
}

func TestIdentityIgnoresExternalRecords(t *testing.T) {
	k := key.NewNode().Public()
	body := `{"devices":[{"isExternal":true},` + identityFixture(k) + `]}`
	got, err := parseIdentity(strings.NewReader(body), time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || len(got) != 1 || got[0].NodePublic != k.String() {
		t.Fatalf("identity = %v, %v", got, err)
	}
}

func TestIdentityRejectsMissingSafetyFields(t *testing.T) {
	fixture := identityFixture(key.NewNode().Public())
	for _, field := range []string{`"authorized":true,`, `"isExternal":false,`, `"keyExpiryDisabled":false,`, `,"expires":"2026-10-04T00:00:00Z"`} {
		t.Run(field, func(t *testing.T) {
			got, err := parseIdentity(strings.NewReader(`{"devices":[`+strings.Replace(fixture, field, "", 1)+`]}`), time.Now())
			if err == nil || got != nil {
				t.Fatalf("missing safety field published identity: %v, %v", got, err)
			}
		})
	}
}

func TestIdentityDoesNotPublishPartialResponse(t *testing.T) {
	device := identityFixture(key.NewNode().Public())
	for _, body := range []string{`{"devices":[` + device, `{"devices":[` + device + `,{}]}`, `{"devices":[` + device + `]} {}`, `{"devices":null}`, `{}`, `{"devices":[],"next":"page2"}`} {
		got, err := parseIdentity(strings.NewReader(body), time.Now())
		if err == nil || got != nil {
			t.Fatalf("partial response published identity: %v, %v", got, err)
		}
	}
}

func TestIdentityFiltersUnauthorizedExpiredAndNormalizesExpiryDisabled(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	k := key.NewNode().Public()
	valid := identityFixture(k)
	disabled := strings.Replace(valid, `"keyExpiryDisabled":false`, `"keyExpiryDisabled":true`, 1)
	disabled = strings.Replace(disabled, `"expires":"2026-10-04T00:00:00Z"`, `"expires":""`, 1)
	body := `{"devices":[` + strings.Replace(valid, `"authorized":true`, `"authorized":false`, 1) + `,` + strings.Replace(valid, "2026-10-04", "2026-10-02", 1) + `,` + disabled + `]}`
	got, err := parseIdentity(strings.NewReader(body), now)
	if err != nil || len(got) != 1 || !got[0].NotAfter.IsZero() {
		t.Fatalf("filtered identity = %v, %v", got, err)
	}
}

func TestIdentityRejectsInvalidDuplicateAndOversized(t *testing.T) {
	valid := identityFixture(key.NewNode().Public())
	for _, body := range []string{`{"devices":[` + valid + `,` + valid + `]}`, `{"devices":[` + strings.Replace(valid, "nodekey:", "invalid:", 1) + `]}`, `{"devices":[],"padding":"` + strings.Repeat("x", 8<<20) + `"}`} {
		got, err := parseIdentity(strings.NewReader(body), time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
		if err == nil || got != nil {
			t.Fatalf("invalid response published identity: %v, %v", got, err)
		}
	}
}
