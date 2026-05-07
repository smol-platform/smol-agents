package agentfs

import "encoding/json"

// readPolicyJSON pulls Mode/VersionID/PointInTime out of any value via
// JSON round-trip. Avoids importing v1.RestorePolicy here, which would
// create a coupling cost not worth the small runtime overhead.
func readPolicyJSON(p any) (mode, versionID, pit string) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", "", ""
	}
	var out struct {
		Mode        string `json:"mode,omitempty"`
		VersionID   string `json:"versionID,omitempty"`
		PointInTime string `json:"pointInTime,omitempty"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Mode, out.VersionID, out.PointInTime
}
