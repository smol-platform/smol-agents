package v1

import (
	"encoding/json"
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

// TestProperty_FeaturesRoundTrip exercises Marshal → Unmarshal → equal
// across arbitrary Features structs. R-OP-FF-5.
func TestProperty_FeaturesRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		f := Features{
			Identity: IdentityFeature{
				FeatureBase:        FeatureBase{Enabled: rapid.Bool().Draw(t, "id-enabled"), RolloutPolicy: pickRolloutPolicy(t)},
				Mode:               pickMode(t),
				WorkloadAPI:        rapid.StringMatching(`unix:///[a-z./]{1,30}`).Draw(t, "workloadAPI"),
				BootTimeoutSeconds: int32(rapid.IntRange(1, 600).Draw(t, "boot")),
			},
			Transport: TransportFeature{
				Private: TransportPrivateFeature{
					FeatureBase: FeatureBase{Enabled: rapid.Bool().Draw(t, "tp-enabled"), RolloutPolicy: pickRolloutPolicy(t)},
					Addr:        rapid.StringMatching(`0\.0\.0\.0:[0-9]{4}`).Draw(t, "tp-addr"),
					Authorize:   rapid.SliceOfN(rapid.StringMatching(`[a-z:./]{5,30}`), 0, 4).Draw(t, "auths"),
				},
				Public: TransportPublicFeature{
					FeatureBase: FeatureBase{Enabled: rapid.Bool().Draw(t, "pub-enabled"), RolloutPolicy: pickRolloutPolicy(t)},
					Addr:        rapid.StringMatching(`0\.0\.0\.0:[0-9]{4}`).Draw(t, "pub-addr"),
					CertPath:    rapid.StringMatching(`/[a-z/]{5,30}`).Draw(t, "cert"),
					KeyPath:     rapid.StringMatching(`/[a-z/]{5,30}`).Draw(t, "key"),
				},
			},
			Secrets: SecretsFeature{
				FeatureBase:        FeatureBase{Enabled: rapid.Bool().Draw(t, "sec-enabled"), RolloutPolicy: pickRolloutPolicy(t)},
				BrokerSocket:       rapid.StringMatching(`/run/[a-z/.]{5,30}`).Draw(t, "sock"),
				MaxLeaseTTLSeconds: int32(rapid.IntRange(1, 86400).Draw(t, "ttl")),
			},
			Sandbox: SandboxFeature{
				FeatureBase:     FeatureBase{Enabled: rapid.Bool().Draw(t, "sb-enabled"), RolloutPolicy: pickRolloutPolicy(t)},
				RuntimeClass:    rapid.SampledFrom([]string{"kata-fc", "gvisor", "kata-qemu"}).Draw(t, "rc"),
				AllowHostEscape: rapid.Bool().Draw(t, "escape"),
			},
			EBPF: EBPFFeature{
				FeatureBase: FeatureBase{Enabled: rapid.Bool().Draw(t, "ebpf-enabled"), RolloutPolicy: pickRolloutPolicy(t)},
				Programs:    rapid.SliceOfN(rapid.StringMatching(`[a-z]{3,10}`), 0, 5).Draw(t, "programs"),
			},
			Knative: KnativeFeature{
				FeatureBase: FeatureBase{Enabled: rapid.Bool().Draw(t, "kn-enabled"), RolloutPolicy: pickRolloutPolicy(t)},
				ScaleToZero: rapid.Bool().Draw(t, "stz"),
				MinScale:    int32(rapid.IntRange(0, 5).Draw(t, "min")),
				MaxScale:    int32(rapid.IntRange(1, 100).Draw(t, "max")),
			},
			Observability: ObservabilityFeature{
				FeatureBase:  FeatureBase{Enabled: rapid.Bool().Draw(t, "obs-enabled"), RolloutPolicy: pickRolloutPolicy(t)},
				OTLPEndpoint: rapid.StringMatching(`[a-z.:0-9]{0,40}`).Draw(t, "otlp"),
				ServiceName:  rapid.StringMatching(`[a-z]{3,15}`).Draw(t, "svcName"),
			},
		}

		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var back Features
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Slices: nil and empty must compare equal for round-trip.
		normaliseFeatures(&f)
		normaliseFeatures(&back)
		if !reflect.DeepEqual(f, back) {
			t.Fatalf("round-trip lost data:\nbefore: %+v\nafter : %+v", f, back)
		}
	})
}

// TestProperty_KnativeAgentRoundTrip — full CR round-trip.
func TestProperty_KnativeAgentRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cr := KnativeAgent{
			Spec: KnativeAgentSpec{
				TrustDomain:    rapid.StringMatching(`[a-z]{3,10}\.[a-z]{2,5}`).Draw(t, "td"),
				Mode:           rapid.SampledFrom([]string{"", "insecure", "permissive", "strict"}).Draw(t, "mode"),
				DeploymentKind: rapid.SampledFrom([]string{"knative", "deployment", "statefulset"}).Draw(t, "dk"),
				Replicas:       int32(rapid.IntRange(1, 10).Draw(t, "replicas")),
			},
		}
		raw, err := json.Marshal(cr)
		if err != nil {
			t.Fatal(err)
		}
		var back KnativeAgent
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if back.Spec.TrustDomain != cr.Spec.TrustDomain ||
			back.Spec.Mode != cr.Spec.Mode ||
			back.Spec.DeploymentKind != cr.Spec.DeploymentKind ||
			back.Spec.Replicas != cr.Spec.Replicas {
			t.Errorf("CR round-trip lost data: %+v vs %+v", cr.Spec, back.Spec)
		}
	})
}

// pickRolloutPolicy yields one of the valid enum values (or empty).
func pickRolloutPolicy(t *rapid.T) string {
	return rapid.SampledFrom([]string{"", "Immediate", "Canary", "Manual"}).Draw(t, "rp")
}

// pickMode yields one of the valid identity modes.
func pickMode(t *rapid.T) string {
	return rapid.SampledFrom([]string{"", "insecure", "permissive", "strict"}).Draw(t, "mode")
}

// normaliseFeatures coerces nil/empty slices to nil so reflect.DeepEqual
// treats them as equal after a round-trip (encoding/json can produce
// either depending on omitempty).
func normaliseFeatures(f *Features) {
	if len(f.Transport.Private.Authorize) == 0 {
		f.Transport.Private.Authorize = nil
	}
	if len(f.EBPF.Programs) == 0 {
		f.EBPF.Programs = nil
	}
}
