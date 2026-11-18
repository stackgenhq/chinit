// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

const (
	defaultMetadataPath = "/etc/chinit/metadata.json"
	defaultRegion       = "local"
	defaultZone         = "compose-1"
)

// MetadataSource resolves container instance metadata from an optional
// JSON file, environment variables, and local network/hostname probes.
type MetadataSource struct {
	region       string
	metadataFile string
	override     *port.InstanceMetadata
}

// NewMetadataSource constructs a Docker Compose metadata source.
func NewMetadataSource(region string) *MetadataSource {
	return &MetadataSource{
		region:       region,
		metadataFile: strings.TrimSpace(os.Getenv("CHINIT_COMPOSE_METADATA_FILE")),
	}
}

// WithMetadata pins fixed metadata (used by conformance and unit tests).
func (m *MetadataSource) WithMetadata(meta port.InstanceMetadata) *MetadataSource {
	cp := meta
	m.override = &cp
	return m
}

// Get returns instance metadata for the container.
func (m *MetadataSource) Get(_ context.Context) (port.InstanceMetadata, error) {
	if m.override != nil {
		out := *m.override
		if out.Provider == "" {
			out.Provider = "compose"
		}
		return out, nil
	}

	meta := port.InstanceMetadata{
		Provider: "compose",
		Region:   firstNonEmpty(os.Getenv("CHINIT_REGION"), m.region, defaultRegion),
		Zone:     firstNonEmpty(os.Getenv("CHINIT_ZONE"), defaultZone),
	}

	path := m.metadataFile
	if path == "" {
		path = defaultMetadataPath
	}
	if data, err := os.ReadFile(path); err == nil {
		var fileMeta struct {
			Region       string `json:"region"`
			Zone         string `json:"zone"`
			InstanceID   string `json:"instance_id"`
			PrivateIP    string `json:"private_ip"`
			HostnameHint string `json:"hostname_hint"`
		}
		if uerr := json.Unmarshal(data, &fileMeta); uerr != nil {
			return port.InstanceMetadata{}, fmt.Errorf("parse %s: %w", path, uerr)
		}
		if fileMeta.Region != "" {
			meta.Region = fileMeta.Region
		}
		if fileMeta.Zone != "" {
			meta.Zone = fileMeta.Zone
		}
		if fileMeta.InstanceID != "" {
			meta.InstanceID = fileMeta.InstanceID
		}
		if fileMeta.PrivateIP != "" {
			meta.PrivateIP = fileMeta.PrivateIP
		}
		if fileMeta.HostnameHint != "" {
			meta.HostnameHint = fileMeta.HostnameHint
		}
	}

	host, _ := os.Hostname()
	meta.HostnameHint = firstNonEmpty(os.Getenv("CHINIT_HOSTNAME"), meta.HostnameHint, host, "compose-node")
	meta.InstanceID = firstNonEmpty(os.Getenv("CHINIT_INSTANCE_ID"), meta.InstanceID, "compose-"+meta.HostnameHint)
	meta.PrivateIP = firstNonEmpty(os.Getenv("CHINIT_PRIVATE_IP"), meta.PrivateIP, firstNonLoopbackIPv4())

	if meta.PrivateIP == "" {
		return port.InstanceMetadata{}, fmt.Errorf("compose metadata: could not determine container private IP (set CHINIT_PRIVATE_IP or %s)", path)
	}
	return meta, nil
}

func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
