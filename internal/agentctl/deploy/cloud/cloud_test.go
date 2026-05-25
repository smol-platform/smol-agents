package cloud

import (
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestCloudInitScript_ContainsExpectedSteps(t *testing.T) {
	got, err := CloudInitScript(CloudInitOptions{Hostname: "agentctl-test"})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	for _, want := range []string{
		"#!/bin/bash",
		"set -euo pipefail",
		"hostnamectl set-hostname agentctl-test",
		"https://get.k0s.sh",
		"k0s install controller --single",
		"systemctl enable --now k0scontroller",
		DefaultKubeconfigPath,
		"touch " + DefaultSentinelPath,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered script missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestCloudInitScript_DefaultPaths(t *testing.T) {
	// Empty Hostname → no hostnamectl line.
	got, err := CloudInitScript(CloudInitOptions{})
	if err != nil {
		t.Fatalf("CloudInitScript: %v", err)
	}
	if strings.Contains(got, "hostnamectl") {
		t.Errorf("expected no hostnamectl line when Hostname is empty; got:\n%s", got)
	}
}

func TestRewriteKubeconfig_RewritesServerAndDropsCA(t *testing.T) {
	// Minimal k0s-shaped kubeconfig: server=localhost, with a fake CA bundle.
	in := []byte(`apiVersion: v1
kind: Config
clusters:
- name: k0s
  cluster:
    server: https://localhost:6443
    certificate-authority-data: ZmFrZQ==
contexts:
- name: k0s
  context: {cluster: k0s, user: admin}
current-context: k0s
users:
- name: admin
  user:
    client-certificate-data: ZmFrZQ==
    client-key-data: ZmFrZQ==
`)
	out, err := RewriteKubeconfig(in, "https://203.0.113.42:6443")
	if err != nil {
		t.Fatalf("RewriteKubeconfig: %v", err)
	}
	cfg, err := clientcmd.Load(out)
	if err != nil {
		t.Fatalf("parse rewritten: %v", err)
	}
	c, ok := cfg.Clusters["k0s"]
	if !ok {
		t.Fatalf("cluster 'k0s' missing after rewrite; got %+v", cfg.Clusters)
	}
	if c.Server != "https://203.0.113.42:6443" {
		t.Errorf("Server = %q, want %q", c.Server, "https://203.0.113.42:6443")
	}
	if !c.InsecureSkipTLSVerify {
		t.Errorf("InsecureSkipTLSVerify = false; expected true after rewrite (k0s CA is bound to localhost)")
	}
	if len(c.CertificateAuthorityData) != 0 {
		t.Errorf("CertificateAuthorityData not cleared")
	}
}

func TestRewriteKubeconfig_RejectsEmpty(t *testing.T) {
	_, err := RewriteKubeconfig([]byte(``), "https://x:6443")
	if err == nil {
		t.Errorf("expected error on empty input; got nil")
	}
}
