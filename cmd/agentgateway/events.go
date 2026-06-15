package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
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
	ev, err := parseCloudEvent(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	name, status, err := g.dispatch(r.Context(), pure.EventTarget{Kind: pure.EventTargetAgentTeam, Name: name}, ns, ev)
	g.writeDispatch(w, ns, ev.ID, name, status, err)
}

// postEvent is the general (EventBinding-routed) CloudEvents intake (epic t0d):
// it lists EventBindings in the namespace, matches them against the event, and
// dispatches to each matched target. Addressable as a Knative Trigger subscriber
// for a whole namespace (vs the per-team /v1/teams/.../events path).
//
//	POST /v1/events/{ns}   (CloudEvents binary or structured mode)
func (g *Gateway) postEvent(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	if g.K8s == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "event intake unavailable (gateway has no cluster client)"})
		return
	}
	ev, err := parseCloudEvent(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var bindings amv1.EventBindingList
	if err := g.K8s.List(r.Context(), &bindings, client.InNamespace(ns)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list eventbindings: " + err.Error()})
		return
	}
	var dispatched []map[string]string
	for i := range bindings.Items {
		b := &bindings.Items[i]
		if !b.Spec.Filter.Matches(ev.Type, ev.Source, ev.Subject) {
			continue
		}
		name, status, derr := g.dispatch(r.Context(), b.Spec.Target, ns, ev)
		entry := map[string]string{"binding": b.Name, "target": string(b.Spec.Target.Kind) + "/" + b.Spec.Target.Name, "status": status}
		if name != "" {
			entry["object"] = name
		}
		if derr != nil {
			entry["error"] = derr.Error()
		}
		dispatched = append(dispatched, entry)
	}
	if len(dispatched) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no EventBinding in " + ns + " matches this event", "eventId": ev.ID})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"namespace": ns, "eventId": ev.ID, "dispatched": dispatched})
}

// dispatch turns a matched event into the target's native work unit and returns
// the created object's name + a status word. Idempotent on the CloudEvent id for
// the run/coordinator targets (named by token); session turns are at-least-once.
func (g *Gateway) dispatch(ctx context.Context, target pure.EventTarget, ns string, ev cloudEvent) (objName, status string, err error) {
	token := eventToken(ev.ID)
	switch target.Kind {
	case pure.EventTargetAgentTeam:
		var team amv1.AgentTeam
		if err := g.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: target.Name}, &team); err != nil {
			return "", "error", wrapNotFound(err, "AgentTeam "+ns+"/"+target.Name)
		}
		run := amv1.BuildCoordinatorRun(&team, token, ev.Data)
		run.Annotations = map[string]string{amv1.CloudEventIDAnnotation: ev.ID}
		return createIdempotent(ctx, g.K8s, run)

	case pure.EventTargetAgent:
		run := &amv1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{
				Name: target.Name + "-" + token, Namespace: ns,
				Annotations: map[string]string{amv1.CloudEventIDAnnotation: ev.ID},
			},
			Spec: pure.AgentRunSpec{AgentRef: target.Name, Input: ev.Data},
		}
		return createIdempotent(ctx, g.K8s, run)

	case pure.EventTargetAgentSession:
		// Post the event as a session turn (the existing durable turn path). The
		// session queue does not dedup, so this is at-least-once (unlike the
		// run/coordinator targets).
		body, _ := json.Marshal(pure.AgentRunSpec{Input: ev.Data})
		id, perr := g.Queue.Publish(ctx, sessionqueue.SessionKey(ns, target.Name), body)
		if perr != nil {
			return "", "error", perr
		}
		return id, "queued", nil

	case pure.EventTargetAgentWorkflow:
		// Per-event AgentWorkflow instantiation needs its own template-vs-instance
		// decision (like the team got); not wired yet (t0d follow-up).
		return "", "unsupported", errors.New("AgentWorkflow target not yet supported")

	default:
		return "", "error", errors.New("unknown target kind " + string(target.Kind))
	}
}

// createIdempotent creates obj; an AlreadyExists (same CloudEvent id) is a
// no-op success so a Knative redelivery is acked without a retry storm.
func createIdempotent(ctx context.Context, c client.Client, obj client.Object) (string, string, error) {
	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return obj.GetName(), "duplicate", nil
		}
		return "", "error", err
	}
	return obj.GetName(), "created", nil
}

func wrapNotFound(err error, what string) error {
	if apierrors.IsNotFound(err) {
		return errors.New(what + " not found")
	}
	return err
}

// writeDispatch renders a single-target dispatch result (the per-team endpoint).
func (g *Gateway) writeDispatch(w http.ResponseWriter, ns, eventID, objName, status string, err error) {
	switch {
	case err != nil && strings.HasSuffix(err.Error(), "not found"):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	case status == "duplicate":
		writeJSON(w, http.StatusOK, map[string]string{"object": objName, "status": "duplicate (idempotent)"})
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"namespace": ns, "object": objName, "eventId": eventID, "status": status})
	}
}

// cloudEvent is the subset of CloudEvents context attributes the gateway routes
// on, plus the data payload.
type cloudEvent struct {
	ID      string
	Type    string
	Source  string
	Subject string
	Data    json.RawMessage
}

// parseCloudEvent extracts a CloudEvent from a request in either the CloudEvents
// HTTP binary mode (ce-* headers; body = data) or structured mode (Content-Type
// application/cloudevents+json; body = the full envelope). A missing id is
// rejected — id is required for idempotency.
func parseCloudEvent(r *http.Request) (cloudEvent, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEventBytes+1))
	if err != nil {
		return cloudEvent{}, errors.New("read event body")
	}
	if int64(len(body)) > maxEventBytes {
		return cloudEvent{}, errors.New("event body exceeds 1 MiB")
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/cloudevents+json") {
		// Structured mode: the whole body is the CloudEvent JSON.
		var ce struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Source  string          `json:"source"`
			Subject string          `json:"subject"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &ce); err != nil {
			return cloudEvent{}, errors.New("structured CloudEvent: invalid JSON: " + err.Error())
		}
		if ce.ID == "" {
			return cloudEvent{}, errors.New("structured CloudEvent: missing id")
		}
		return cloudEvent{ID: ce.ID, Type: ce.Type, Source: ce.Source, Subject: ce.Subject, Data: ce.Data}, nil
	}

	// Binary mode: attributes are ce-* headers, the body is the data.
	id := r.Header.Get("Ce-Id")
	if id == "" {
		return cloudEvent{}, errors.New("CloudEvent: missing ce-id header (and not structured mode)")
	}
	return cloudEvent{
		ID:      id,
		Type:    r.Header.Get("Ce-Type"),
		Source:  r.Header.Get("Ce-Source"),
		Subject: r.Header.Get("Ce-Subject"),
		Data:    json.RawMessage(body),
	}, nil
}

// eventToken derives a deterministic, DNS-1123-safe label segment from a raw
// CloudEvent id (which may be a UUID, URI, or arbitrary string). The same id
// always maps to the same token, so the coordinator run name is idempotent; the
// raw id is preserved in the run's CloudEventIDAnnotation.
func eventToken(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8]) // 16 hex chars, always a valid name segment
}
