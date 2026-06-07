package invokers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

type fakeLeaser struct {
	val string
	err error
}

func (f fakeLeaser) LeaseSecret(_ context.Context, _ string, _ time.Duration) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.val), nil
}

func httpTool(url string, auth *v1.AuthRef, headers map[string]string) v1.Tool {
	return v1.Tool{Name: "search", Spec: v1.ToolSpec{
		Kind: v1.ToolHTTP,
		HTTP: &v1.HTTPSpec{URL: url, Auth: auth, Headers: headers},
	}}
}

func TestHTTPInvoker_PostsArgsAppliesAuthAndHeaders(t *testing.T) {
	var gotBody, gotAuth, gotHdr, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotAuth, gotHdr, gotCT = string(b), r.Header.Get("Authorization"), r.Header.Get("X-Extra"), r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"hits":3}`))
	}))
	defer srv.Close()

	inv := &HTTPInvoker{Client: srv.Client(), Leaser: fakeLeaser{val: "tok-123"}}
	obs, err := inv.Invoke(context.Background(),
		httpTool(srv.URL, &v1.AuthRef{SecretName: "tool-key"}, map[string]string{"X-Extra": "y"}),
		json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotBody != `{"q":"x"}` {
		t.Errorf("body = %q", gotBody)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("auth = %q, want Bearer tok-123", gotAuth)
	}
	if gotHdr != "y" || gotCT != "application/json" {
		t.Errorf("headers wrong: extra=%q ct=%q", gotHdr, gotCT)
	}
	if string(obs.Output) != `{"hits":3}` {
		t.Errorf("observation = %s", obs.Output)
	}
}

func TestHTTPInvoker_Non2xxAndNonJSON(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"e":1}`))
	}))
	defer bad.Close()
	inv := &HTTPInvoker{Client: bad.Client()}
	if _, err := inv.Invoke(context.Background(), httpTool(bad.URL, nil, nil), json.RawMessage(`{}`)); err == nil {
		t.Errorf("non-2xx must error")
	}

	notjson := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer notjson.Close()
	inv2 := &HTTPInvoker{Client: notjson.Client()}
	if _, err := inv2.Invoke(context.Background(), httpTool(notjson.URL, nil, nil), json.RawMessage(`{}`)); err == nil {
		t.Errorf("non-JSON body must error")
	}
}

func TestHTTPInvoker_AuthWithoutLeaser(t *testing.T) {
	inv := &HTTPInvoker{} // no leaser
	_, err := inv.Invoke(context.Background(), httpTool("http://x", &v1.AuthRef{SecretName: "k"}, nil), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "no secret leaser") {
		t.Errorf("auth without leaser must error, got %v", err)
	}
}

func TestHTTPInvoker_OverCap(t *testing.T) {
	big := strings.Repeat("a", (256<<10)+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"x":"` + big + `"}`))
	}))
	defer srv.Close()
	inv := &HTTPInvoker{Client: srv.Client()}
	if _, err := inv.Invoke(context.Background(), httpTool(srv.URL, nil, nil), json.RawMessage(`{}`)); err == nil {
		t.Errorf("over-cap response must error")
	}
}
