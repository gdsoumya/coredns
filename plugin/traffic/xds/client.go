/*
This package contains code copied from github.com/grpc/grpc-co. The license for that code is:

Copyright 2019 gRPC authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package xds implements a bidirectional stream to an envoy ADS management endpoint. It will stream
// updates (CDS and EDS) from there to help load balance responses to DNS clients.
package xds

import (
	"context"
	"sync"

	clog "github.com/coredns/coredns/plugin/pkg/log"

	xdspb "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	corepb "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	adsgrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/golang/protobuf/ptypes"
	"google.golang.org/grpc"
)

var log = clog.NewWithPlugin("traffic")

const (
	cdsURL = "type.googleapis.com/envoy.api.v2.Cluster"
	edsURL = "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment"
)

type adsStream adsgrpc.AggregatedDiscoveryService_StreamAggregatedResourcesClient

type Client struct {
	cc          *grpc.ClientConn
	ctx         context.Context
	assignments assignment
	node        *corepb.Node
	cancel      context.CancelFunc
}

type assignment struct {
	mu      sync.RWMutex
	cla     map[string]*xdspb.ClusterLoadAssignment
	version int // not sure what do with and if we should discard all clusters.
}

func (a assignment) SetClusterLoadAssignment(cluster string, cla *xdspb.ClusterLoadAssignment) {
	// if cla is nil we just found a cluster, check if we already know about it, or if we need to make
	// a new entry
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.cla[cluster]
	if !ok {
		a.cla[cluster] = cla
		return
	}
	if cla == nil {
		return
	}
	a.cla[cluster] = cla

}

func (a assignment) ClusterLoadAssignment(cluster string) *xdspb.ClusterLoadAssignment {
	return nil
}

func (a assignment) Clusters() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	clusters := make([]string, len(a.cla))
	i := 0
	for k := range a.cla {
		clusters[i] = k
		i++
	}
	return clusters
}

// New returns a new client that's dialed to addr using node as the local identifier.
func New(addr, node string) (*Client, error) {
	// todo credentials
	opts := []grpc.DialOption{grpc.WithInsecure()}
	cc, err := grpc.Dial(addr, opts...)
	if err != nil {
		return nil, err
	}
	c := &Client{cc: cc, node: &corepb.Node{Id: "test-id"}} // do more with this node data? Hostname port??
	c.assignments = assignment{cla: make(map[string]*xdspb.ClusterLoadAssignment)}
	c.ctx, c.cancel = context.WithCancel(context.Background())

	return c, nil
}

func (c *Client) Close() { c.cancel(); c.cc.Close() }

func (c *Client) Run() (adsgrpc.AggregatedDiscoveryService_StreamAggregatedResourcesClient, error) {
	cli := adsgrpc.NewAggregatedDiscoveryServiceClient(c.cc)
	stream, err := cli.StreamAggregatedResources(c.ctx)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (c *Client) ClusterDiscovery(stream adsStream, version, nonce string, clusters []string) error {
	req := &xdspb.DiscoveryRequest{
		Node:          c.node,
		TypeUrl:       cdsURL,
		ResourceNames: clusters, // empty for all
		VersionInfo:   version,
		ResponseNonce: nonce,
	}
	return stream.Send(req)
}

func (c *Client) EndpointDiscovery(stream adsStream, version, nonce string, clusters []string) error {
	req := &xdspb.DiscoveryRequest{
		Node:          c.node,
		TypeUrl:       edsURL,
		ResourceNames: clusters,
		VersionInfo:   version,
		ResponseNonce: nonce,
	}
	return stream.Send(req)
}

func (c *Client) Receive(stream adsStream) error {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}

		switch resp.GetTypeUrl() {
		case cdsURL:
			for _, r := range resp.GetResources() {
				var any ptypes.DynamicAny
				if err := ptypes.UnmarshalAny(r, &any); err != nil {
					continue
				}
				cluster, ok := any.Message.(*xdspb.Cluster)
				if !ok {
					continue
				}
				c.assignments.SetClusterLoadAssignment(cluster.GetName(), nil)
			}
			println("HERER", len(resp.GetResources()))
			log.Debug("Cluster discovery processed with %d resources", len(resp.GetResources()))
			// ack the CDS proto, with we we've got. (empty version would be NACK)
			if err := c.ClusterDiscovery(stream, resp.GetVersionInfo(), resp.GetNonce(), c.assignments.Clusters()); err != nil {
				log.Warningf("Failed to acknowledge cluster discovery: %s", err)
			}
			// need to figure out how to handle the version exactly.

			// now kick off discovery for endpoints
			if err := c.EndpointDiscovery(stream, "", "", c.assignments.Clusters()); err != nil {
				log.Warningf("Failed to perform endpoint discovery: %s", err)
			}

		case edsURL:
			println("EDS")
		default:
			log.Warningf("Unknown response URL for discovery: %q", resp.GetTypeUrl())
			continue
		}
	}
	return nil
}
