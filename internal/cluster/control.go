package cluster

import (
	"errors"
	"time"
)

var ErrIdentityConflict = errors.New("node identity conflict")
var ErrHostBudget = errors.New("node host budget exceeded")

type ControlMessage struct {
	Type   string  `json:"type"`
	Policy *Policy `json:"policy,omitempty"`
}

type PolicyACK struct {
	Revision uint64 `json:"revision"`
	State    string `json:"state"`
	Usable   bool   `json:"usable,omitempty"`
	Error    string `json:"error,omitempty"`
}

type PolicyApplication struct {
	Revision          uint64
	Usable            bool
	Traffic           []TailnetTraffic
	TrafficObservedAt time.Time
}

type TailnetTraffic struct {
	TailnetID           string `json:"tailnet_id"`
	RXPayloadBytes      uint64 `json:"rx_payload_bytes"`
	TXPayloadBytes      uint64 `json:"tx_payload_bytes"`
	RelayedPayloadBytes uint64 `json:"relayed_payload_bytes"`
	QueuedPayloadBytes  uint64 `json:"queued_payload_bytes"`
}

const MaxControlMessageBytes = MaxPolicyBytes + 512

type NodeHeartbeat struct {
	NodeID                  string    `json:"node_id"`
	LeaseUntil              time.Time `json:"lease_until"`
	DesiredRevision         uint64    `json:"desired_revision"`
	HeartbeatIntervalMillis int64     `json:"heartbeat_interval_millis"`
}
