package v1

import "testing"

func TestValidateEventBinding(t *testing.T) {
	cases := []struct {
		name    string
		spec    EventBindingSpec
		wantErr bool
	}{
		{"valid team target", EventBindingSpec{Target: EventTarget{Kind: EventTargetAgentTeam, Name: "squad"}}, false},
		{"valid agent target", EventBindingSpec{Target: EventTarget{Kind: EventTargetAgent, Name: "a"}}, false},
		{"missing name", EventBindingSpec{Target: EventTarget{Kind: EventTargetAgent}}, true},
		{"missing kind", EventBindingSpec{Target: EventTarget{Name: "a"}}, true},
		{"bad kind", EventBindingSpec{Target: EventTarget{Kind: "Pod", Name: "a"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateEventBinding(c.spec)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateEventBinding(%+v) err=%v, wantErr=%v", c.spec, err, c.wantErr)
			}
		})
	}
}

func TestEventFilter_Matches(t *testing.T) {
	cases := []struct {
		name                     string
		f                        EventFilter
		ceType, ceSource, ceSubj string
		want                     bool
	}{
		{"empty filter matches any", EventFilter{}, "x", "y", "z", true},
		{"type match", EventFilter{Type: "t"}, "t", "", "", true},
		{"type mismatch", EventFilter{Type: "t"}, "other", "", "", false},
		{"all three match", EventFilter{Type: "t", Source: "s", Subject: "j"}, "t", "s", "j", true},
		{"subject mismatch", EventFilter{Type: "t", Subject: "j"}, "t", "", "other", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.Matches(c.ceType, c.ceSource, c.ceSubj); got != c.want {
				t.Errorf("Matches = %v, want %v", got, c.want)
			}
		})
	}
}
