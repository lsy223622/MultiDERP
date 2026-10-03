package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"tailscale.com/types/key"
)

type identityKey struct {
	NodePublic string
	NotAfter   time.Time
}

func parseIdentity(r io.Reader, now time.Time) ([]identityKey, error) {
	const maxResponse = 8 << 20
	body, err := io.ReadAll(io.LimitReader(r, maxResponse+1))
	if err != nil || len(body) > maxResponse {
		return nil, errors.New("device response unreadable or too large")
	}
	var response struct {
		Devices json.RawMessage `json:"devices"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&response); err != nil {
		return nil, errors.New("invalid complete device response")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing device response data")
	}
	var devices []struct {
		NodeKey           string  `json:"nodeKey"`
		Authorized        *bool   `json:"authorized"`
		IsExternal        *bool   `json:"isExternal"`
		KeyExpiryDisabled *bool   `json:"keyExpiryDisabled"`
		Expires           *string `json:"expires"`
	}
	if len(response.Devices) == 0 || string(response.Devices) == "null" || json.Unmarshal(response.Devices, &devices) != nil {
		return nil, errors.New("missing complete device list")
	}
	result := make([]identityKey, 0, len(devices))
	seen := make(map[key.NodePublic]bool)
	for i, d := range devices {
		if d.IsExternal == nil {
			return nil, fmt.Errorf("device %d: missing external status", i)
		}
		if *d.IsExternal {
			continue
		}
		if d.Authorized == nil || d.KeyExpiryDisabled == nil || d.Expires == nil {
			return nil, fmt.Errorf("device %d: missing safety fields", i)
		}
		var k key.NodePublic
		if err := k.UnmarshalText([]byte(d.NodeKey)); err != nil || k.IsZero() {
			return nil, fmt.Errorf("device %d: invalid node public key", i)
		}
		var expiry time.Time
		if !*d.KeyExpiryDisabled {
			expiry, err = time.Parse(time.RFC3339, *d.Expires)
			if err != nil || expiry.IsZero() {
				return nil, fmt.Errorf("device %d: invalid key expiry", i)
			}
		}
		if !*d.Authorized || (!expiry.IsZero() && !now.Before(expiry)) {
			continue
		}
		if seen[k] {
			return nil, fmt.Errorf("device %d: duplicate public key", i)
		}
		seen[k] = true
		result = append(result, identityKey{NodePublic: k.String(), NotAfter: expiry})
	}
	return result, nil
}
