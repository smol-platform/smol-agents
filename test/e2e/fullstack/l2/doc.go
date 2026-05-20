// Package l2 hosts the single-EC2-k0s-on-AWS ring. Build tag: e2e_l2.
//
// Provisions a Spot c6gd.metal in us-east-2 (stigen account), boots
// k0s + Kata via cloud-init, runs every applicable scenario, and
// terminates the instance on exit. See
// .spec-workflow/specs/smol-agents-fullstack-e2e/design.md for the
// full topology.
package l2
