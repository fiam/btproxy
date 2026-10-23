package esphome

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// textSensor is a text sensor entity exposed to Home Assistant. State is
// pulled from State on every push.
type textSensor struct {
	objectID string
	key      uint32
	name     string
	icon     string
	state    func() string
}

func (t *textSensor) listResponse() ListEntitiesTextSensorResponse {
	return ListEntitiesTextSensorResponse{
		ObjectID: t.objectID,
		Key:      t.key,
		Name:     t.name,
		Icon:     t.icon,
	}
}

func (t *textSensor) stateResponse() TextSensorStateResponse {
	return TextSensorStateResponse{
		Key:   t.key,
		State: t.state(),
	}
}

// numericSensor is a numeric sensor entity; State returns a float.
type numericSensor struct {
	objectID string
	key      uint32
	name     string
	icon     string
	unit     string
	// stateClass: 2 = TOTAL, 3 = TOTAL_INCREASING (api.proto SensorStateClass).
	stateClass     uint32
	accuracyDigits uint32
	state          func() float32
}

func (n *numericSensor) listResponse() ListEntitiesSensorResponse {
	resp := ListEntitiesSensorResponse{
		ObjectID: n.objectID,
		Key:      n.key,
		Name:     n.name,
		Icon:     n.icon,
	}
	if n.unit != "" {
		resp.UnitOfMeasurement = n.unit
	}
	if n.accuracyDigits != 0 {
		resp.AccuracyDecimals = n.accuracyDigits
	}
	if n.stateClass != 0 {
		resp.StateClass = n.stateClass
	}
	return resp
}

func (n *numericSensor) stateResponse() SensorStateResponse {
	return SensorStateResponse{
		Key:   n.key,
		State: n.state(),
	}
}

// defaultTextSensors returns the text entities btproxy always exposes.
func (s *Server) defaultTextSensors() []textSensor {
	return []textSensor{
		{
			objectID: "uptime",
			key:      TextSensorKeyUptime,
			name:     "Uptime",
			icon:     "mdi:clock-outline",
			state:    func() string { return FormatUptime(time.Since(s.startedAt)) },
		},
	}
}

// defaultSensors returns the numeric entities btproxy always exposes.
func (s *Server) defaultSensors() []numericSensor {
	return []numericSensor{
		{
			objectID: "advertisements",
			key:      SensorKeyAdvertisements,
			name:     "Advertisements",
			icon:     "mdi:bluetooth-searching",
			unit:     "adv",
			// ESPHome SensorStateClass TOTAL_INCREASING: a monotonically
			// growing counter since process start.
			stateClass: 3,
			state:      func() float32 { return float32(s.advertisementsSent()) },
		},
		{
			objectID: "connected_clients",
			key:      SensorKeyConnectedClients,
			name:     "Connected Clients",
			icon:     "mdi:account-network",
			// ESPHome SensorStateClass MEASUREMENT: an instantaneous value.
			stateClass: 1,
			state:      func() float32 { return float32(s.connectedClients()) },
		},
	}
}

// advertisementsSent reports how many raw advertisements have been
// forwarded to subscribers since process start.
func (s *Server) advertisementsSent() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.advsForwarded
}

// connectedClients reports how many unique client hosts (by remote
// address) are currently connected, including unauthenticated ones. A
// client that opens several connections — Home Assistant does — counts
// once.
func (s *Server) connectedClients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	hosts := make(map[string]struct{}, len(s.sessions))
	for sess := range s.sessions {
		remote := sess.conn.RemoteAddr().String()
		if host, _, err := net.SplitHostPort(remote); err == nil {
			remote = host
		}
		hosts[remote] = struct{}{}
	}
	return len(hosts)
}

// FormatUptime renders a duration ESPHome-style: "1d 2h 3m 45s", dropping
// leading zero units ("3m 45s", "45s").
func FormatUptime(d time.Duration) string {
	var parts []string
	total := int(d.Seconds())
	days := total / 86400
	hours := (total % 86400) / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 || days > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 || hours > 0 || days > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	parts = append(parts, fmt.Sprintf("%ds", seconds))
	return strings.Join(parts, " ")
}
