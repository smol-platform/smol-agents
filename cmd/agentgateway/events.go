package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// maxEventBytes caps a CloudEvent body (the coordinator input). Matches the
// session turn-cap spirit; over-limit is rejected, not truncated.
const maxEventBytes = 1 << 20 // 1 MiB

// postTeamEvent is the CloudEvents intake for an event-driven AgentTeam (epic
// t0d / rv3.1): each inbound event instantiates one fresh coordinator run of the
// team's lead (Knative-function style). Idempotent on the CloudEvent id, so an
// at-least-once redelivery is a no-op. The gateway is an addressable Knative
// Service, so a Knative Trigger can name it as the subscriber.
//
//	POST /v1/teams/{ns}/{name}/events   (CloudEvents binary or structured mode)
func (g *Gateway) postTeamEvent(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if g.K8s == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "event intake unavailable (gateway has no cluster client)"})
		return
	}
	id, data, err := parseCloudEvent(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var team amv1.AgentTeam
	if err := g.K8s.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &team); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "AgentTeam " + ns + "/" + name + " not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "get team: " + err.Error()})
		return
	}

	run := amv1.BuildCoordinatorRun(&team, eventToken(id), data)
	run.Annotations = map[string]string{amv1.CloudEventIDAnnotation: id}
	if err := g.K8s.Create(r.Context(), run); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Idempotent: this event id already has a coordinator. 200, not error,
			// so a Knative redelivery is acked (no retry storm).
			writeJSON(w, http.StatusOK, map[string]string{"run": run.Name, "status": "duplicate (idempotent)"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create coordinator run: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"namespace": ns, "team": name, "run": run.Name, "eventId": id})
}

// parseCloudEvent extracts the CloudEvent id + data from a request in either the
// CloudEvents HTTP binary mode (ce-* headers; body = data) or structured mode
// (Content-Type application/cloudevents+json; body = the full envelope). A
// missing id is rejected — id is required for idempotency.
func parseCloudEvent(r *http.Request) (id string, data json.RawMessage, err error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEventBytes+1))
	if err != nil {
		return "", nil, errors.New("read event body")
	}
	if int64(len(body)) > maxEventBytes {
		return "", nil, errors.New("event body exceeds 1 MiB")
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/cloudevents+json") {
		// Structured mode: the whole body is the CloudEvent JSON.
		var ce struct {
			ID   string          `json:"id"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &ce); err != nil {
			return "", nil, errors.New("structured CloudEvent: invalid JSON: " + err.Error())
		}
		if ce.ID == "" {
			return "", nil, errors.New("structured CloudEvent: missing id")
		}
		return ce.ID, ce.Data, nil
	}

	// Binary mode: attributes are ce-* headers, the body is the data.
	id = r.Header.Get("Ce-Id")
	if id == "" {
		return "", nil, errors.New("CloudEvent: missing ce-id header (and not structured mode)")
	}
	return id, json.RawMessage(body), nil
}

// eventToken derives a deterministic, DNS-1123-safe label segment from a raw
// CloudEvent id (which may be a UUID, URI, or arbitrary string). The same id
// always maps to the same token, so the coordinator run name is idempotent; the
// raw id is preserved in the run's CloudEventIDAnnotation.
func eventToken(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8]) // 16 hex chars, always a valid name segment
}
