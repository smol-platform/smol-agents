package ebpfloader

import "testing"

func TestParseKernelVersion(t *testing.T) {
	cases := []struct {
		in           string
		major, minor int
	}{
		{"5.15.0-100-generic", 5, 15},
		{"6.6.13-arch1-1", 6, 6},
		{"5.8.0", 5, 8},
		{"4.19.0+", 4, 19},
		{"", 0, 0},
		{"garbage", 0, 0},
	}
	for _, c := range cases {
		mj, mn := parseKernelVersion(c.in)
		if mj != c.major || mn != c.minor {
			t.Errorf("parseKernelVersion(%q) = %d.%d, want %d.%d", c.in, mj, mn, c.major, c.minor)
		}
	}
}

func TestRingBufferSupport(t *testing.T) {
	cases := map[string]bool{
		"5.15.0": true,
		"5.8.0":  true,
		"5.7.10": false,
		"4.19.0": false,
		"6.0.0":  true,
		"":       false,
	}
	for k, want := range cases {
		got := detectRingBufferSupport(k)
		if got != want {
			t.Errorf("detectRingBufferSupport(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestKernelFeaturesString(t *testing.T) {
	f := KernelFeatures{UnameRelease: "5.15", BTFAvailable: true, HasRingBuffer: true, BPFFSPath: "/sys/fs/bpf"}
	got := f.String()
	want := "kernel=5.15 btf=true ringbuf=true bpffs=/sys/fs/bpf"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
