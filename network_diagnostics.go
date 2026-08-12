package main

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RuntimeKind describes the detected container runtime environment.
type RuntimeKind string

const (
	RuntimeDocker    RuntimeKind = "docker"
	RuntimeContainer RuntimeKind = "container"
	RuntimeHost      RuntimeKind = "host"
)

// DockerNetwork describes the detected Docker network mode (best-effort).
type DockerNetwork string

const (
	DockerNetUnknown DockerNetwork = ""
	DockerNetBridge  DockerNetwork = "bridge"
	DockerNetHost    DockerNetwork = "host"
	DockerNetMacvlan DockerNetwork = "macvlan"
)

// NetworkDiagnostics captures the detected runtime environment presented in the
// diagnostics panel.
type NetworkDiagnostics struct {
	Runtime        RuntimeKind        `json:"runtime"`
	DockerNetwork  DockerNetwork      `json:"docker_network"`
	NetAdmin       bool               `json:"net_admin"`
	Interfaces     []NetworkInterface `json:"interfaces"`
	DefaultGateway string             `json:"default_gateway"` // "Unknown" if detection failed
	Recommendation string             `json:"recommendation"`  // advisory text
	RecommendedMode string            `json:"recommended_mode"` // default | gateway | proxy | ""
	CheckedAt      time.Time          `json:"checked_at"`
}

// collectDiagnostics runs all probes and returns a consolidated snapshot.
// All helpers are best-effort — failures fall back to "Unknown" rather than erroring.
func collectDiagnostics(ctx context.Context) NetworkDiagnostics {
	ifaces, _ := listNetworkInterfaces()

	d := NetworkDiagnostics{
		Runtime:       detectRuntime(),
		DockerNetwork: DockerNetUnknown,
		NetAdmin:      checkNetAdmin(),
		Interfaces:    ifaces,
		CheckedAt:     time.Now(),
	}
	if d.Runtime == RuntimeDocker {
		d.DockerNetwork = detectDockerNetworkMode(ifaces)
	}
	d.DefaultGateway = detectDefaultGateway(ctx)
	d.RecommendedMode, d.Recommendation = recommendEgress(d)
	return d
}

// detectRuntime returns docker/container/host based on filesystem markers.
// Best-effort — no error path; falls back to host if nothing matches.
func detectRuntime() RuntimeKind {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return RuntimeDocker
	}
	// Container hints in cgroup (docker, containerd, podman, kubepods).
	if f, err := os.Open("/proc/1/cgroup"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "docker") || strings.Contains(line, "containerd") {
				return RuntimeDocker
			}
			if strings.Contains(line, "kubepods") || strings.Contains(line, "podman") {
				return RuntimeContainer
			}
		}
	}
	return RuntimeHost
}

// detectDockerNetworkMode infers the Docker network mode from interface
// addresses. This is a heuristic — macvlan containers typically have a
// "real" LAN IP on eth0 (e.g., 192.168.x.x matching the host's subnet),
// while bridge containers sit on 172.16.0.0/12 or the default docker0 range.
func detectDockerNetworkMode(ifaces []NetworkInterface) DockerNetwork {
	for _, iface := range ifaces {
		for _, addr := range iface.Addresses {
			ip := net.ParseIP(addr)
			if ip == nil || ip.To4() == nil {
				continue
			}
			// 172.16.0.0/12 and 10.0.0.0/8 common bridge ranges.
			// 192.168.x.x on eth0 in a container strongly suggests macvlan.
			if iface.Name == "eth0" && strings.HasPrefix(addr, "192.168.") {
				return DockerNetMacvlan
			}
			if strings.HasPrefix(addr, "172.") {
				return DockerNetBridge
			}
		}
	}
	return DockerNetUnknown
}

// detectDefaultGateway shells out to `ip route show default` to discover the
// first IPv4 default gateway. Returns empty string on any failure.
//
// Output format: "default via 192.168.x.1 dev eth0 proto dhcp src 192.168.x.y ..."
func detectDefaultGateway(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ip", "-4", "route", "show", "default")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		// Expect: default via <gateway> dev <iface> ...
		for i, f := range fields {
			if f == "via" && i+1 < len(fields) {
				gw := fields[i+1]
				if ip := net.ParseIP(gw); ip != nil && ip.To4() != nil {
					return gw
				}
			}
		}
	}
	return ""
}

// recommendEgress returns a mode suggestion and advisory text. Never auto-selects
// for the user — the UI treats this as a hint only.
func recommendEgress(d NetworkDiagnostics) (mode string, text string) {
	switch {
	case d.Runtime == RuntimeDocker && d.DockerNetwork == DockerNetMacvlan && d.NetAdmin:
		return "gateway", "Docker macvlan with NET_ADMIN detected — use 'Specific gateway' mode to route through different gateways on the same subnet."
	case d.Runtime == RuntimeDocker && d.DockerNetwork == DockerNetBridge && !d.NetAdmin:
		return "proxy", "Docker bridge mode without NET_ADMIN — use 'HTTP proxy' mode for routing through a specific gateway via a proxy."
	case d.NetAdmin:
		return "default", "NET_ADMIN available — 'Default route' mode works for single-gateway setups. 'Specific gateway' mode is available if you need per-WAN routing."
	default:
		return "default", "Use 'Default route' mode. NET_ADMIN is not available, so gateway mode cannot install policy-based routing rules."
	}
}
