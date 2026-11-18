// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// PrivateDNS manages the node's A record in an Azure Private DNS zone
// via the ARM REST surface (the same API az network dns record-set
// called; the bearer is the managed identity token for ARM).
type PrivateDNS struct {
	client      httpClient
	TokenSource func(ctx context.Context, resource string) (string, error)
}

// NewPrivateDNS builds the registrar.
func NewPrivateDNS(_ *model.BootSpec) *PrivateDNS {
	return &PrivateDNS{
		client: http.DefaultClient,
		TokenSource: func(ctx context.Context, resource string) (string, error) {
			return managedIdentityToken(ctx, http.DefaultClient, resource)
		},
	}
}

// armScope is the ARM resource-id template: subscription/resource-group
// arrive via environment (the spec keeps provider-neutral; the wrapper
// user-data can pin them for Azure).
func armScope(zoneName string) (string, error) {
	sub := strings.TrimSpace(os.Getenv("CHINIT_AZURE_SUBSCRIPTION_ID"))
	rg := strings.TrimSpace(os.Getenv("CHINIT_AZURE_RESOURCE_GROUP"))
	if sub == "" || rg == "" {
		return "", fmt.Errorf("azure ARM scope not configured (CHINIT_AZURE_SUBSCRIPTION_ID, CHINIT_AZURE_RESOURCE_GROUP)")
	}
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/privateDnsZones/%s",
		sub, rg, zoneName), nil
}

// ResolveZoneID returns the ARM id of the private DNS zone by name.
// Azure's ARM id IS the zone id (Route53 needed a list+match; here the
// scope is deterministic once subscription/rg are known).
func (p *PrivateDNS) ResolveZoneID(ctx context.Context, zoneName string) (string, error) {
	return armScope(zoneName)
}

// recordValue returns the first IPv4 the named A record currently
// carries ("" when the record is absent) — the ExactOnly delete's
// ownership test (a mismatch vs the caller's IP means a replacement
// re-registered the record).
func (p *PrivateDNS) recordValue(ctx context.Context, scope, name string) (string, error) {
	url := fmt.Sprintf("https://management.azure.com%s/A/%s?api-version=2020-06-01", scope, name)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	token, err := p.TokenSource(ctx, "https://management.azure.com/")
	if err != nil {
		return "", fmt.Errorf("managed identity token (ARM): %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil // absent record: nothing to own
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get A %q: status %d", name, resp.StatusCode)
	}
	var doc recordSetDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("get A %q: decode: %w", name, err)
	}
	for _, rec := range doc.Properties.ARecords {
		return rec.IPv4Address, nil
	}
	return "", nil
}

// recordSetDoc is the PUT body for the A record set.
type recordSetDoc struct {
	Properties struct {
		TTL      int    `json:"ttl"`
		ARecords []aRec `json:"aRecords"`
	} `json:"properties"`
}

type aRec struct {
	IPv4Address string `json:"ipv4Address"`
}

// UpsertARecord creates or replaces the node's A record (ARM PUT on the
// record-set path is create-or-replace by construction).
func (p *PrivateDNS) UpsertARecord(ctx context.Context, req chinitport.ARecordRequest) error {
	if req.TTL == 0 {
		req.TTL = 60
	}
	scope, err := armScope(req.ZoneName)
	if err != nil {
		return err
	}
	var doc recordSetDoc
	doc.Properties.TTL = req.TTL
	doc.Properties.ARecords = []aRec{{IPv4Address: req.IP}}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://management.azure.com%s/A/%s?api-version=2020-06-01", scope, req.RecordName)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token, err := p.TokenSource(ctx, "https://management.azure.com/")
	if err != nil {
		return fmt.Errorf("managed identity token (ARM): %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("upsert A %q: status %d", req.RecordName, resp.StatusCode)
	}
	return nil
}

// DeleteARecord removes the node's A record during drain or termination.
// When ExactOnly is set, the current record value is verified against
// req.IP before deletion so a replacement instance's record is preserved.
func (p *PrivateDNS) DeleteARecord(ctx context.Context, req chinitport.ARecordRequest) error {
	scope, err := armScope(req.ZoneName)
	if err != nil {
		return err
	}
	if req.ExactOnly {
		own, err := p.recordValue(ctx, scope, req.RecordName)
		if err != nil {
			return err
		}
		if own != "" && req.IP != "" && own != req.IP {
			return fmt.Errorf("delete A %q refused: record carries %q, caller is %q (a replacement re-registered it)",
				req.RecordName, own, req.IP)
		}
	}
	url := fmt.Sprintf("https://management.azure.com%s/A/%s?api-version=2020-06-01", scope, req.RecordName)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	token, err := p.TokenSource(ctx, "https://management.azure.com/")
	if err != nil {
		return fmt.Errorf("managed identity token (ARM): %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete A %q: status %d", req.RecordName, resp.StatusCode)
	}
	return nil
}
