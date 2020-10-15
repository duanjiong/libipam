// Copyright (c) 2016-2020 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipam

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"runtime"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/libcalico-go/lib/set"
	"kubesphere.io/libipam/lib/apis/v1alpha1"

	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/names"
	"github.com/projectcalico/libcalico-go/lib/net"
	bapi "kubesphere.io/libipam/lib/backend/api"
	"kubesphere.io/libipam/lib/backend/model"
)

const (
	// Number of retries when we have an error writing data
	// to etcd.
	datastoreRetries  = 100
	ipamKeyErrRetries = 3

	// Common attributes which may be set on allocations by clients.  Moved to the model package so they can be used
	// by the AllocationBlock code too.
	AttributePod           = model.IPAMBlockAttributePod
	AttributeVm            = model.IPAMBlockAttributeVm
	AttributeNamespace     = model.IPAMBlockAttributeNamespace
	AttributeNode          = model.IPAMBlockAttributeNode
	AttributeType          = model.IPAMBlockAttributeType
	AttributeTypeIPIP      = model.IPAMBlockAttributeTypeIPIP
	AttributeTypeVXLAN     = model.IPAMBlockAttributeTypeVXLAN
	AttributeTypeWireguard = model.IPAMBlockAttributeTypeWireguard
)

var (
	ErrBlockLimit      = errors.New("cannot allocate new block due to per host block limit")
	ErrNoQualifiedPool = errors.New("cannot find a qualified ippool")
	ErrStrictAffinity  = errors.New("global strict affinity should not be false for Windows node")
)

// NewIPAMClient returns a new ipamClient, which implements Interface.
// Consumers of the Calico API should not create this directly, but should
// access IPAM through the main client IPAM accessor (e.g. clientv1alpha1.IPAM())
func NewIPAMClient(client bapi.Client, pools PoolAccessorInterface) Interface {
	return &ipamClient{
		client: client,
		pools:  pools,
		blockReaderWriter: blockReaderWriter{
			client: client,
			pools:  pools,
		},
	}
}

// ipamClient implements Interface
type ipamClient struct {
	client            bapi.Client
	pools             PoolAccessorInterface
	blockReaderWriter blockReaderWriter
}

// AutoAssign automatically assigns one or more IP addresses as specified by the
// provided AutoAssignArgs.  AutoAssign returns the list of the assigned IPv4 addresses,
// and the list of the assigned IPv6 addresses.
//
// In case of error, returns the IPs allocated so far along with the error.
func (c ipamClient) AutoAssign(ctx context.Context, args AutoAssignArgs) ([]net.IPNet, []net.IPNet, error) {
	// Determine the hostname to use - prefer the provided hostname if
	// non-nil, otherwise use the hostname reported by os.
	hostname, err := decideHostname(args.Hostname)
	if err != nil {
		return nil, nil, err
	}
	log.Infof("Auto-assign %d ipv4, %d ipv6 addrs for host '%s'", args.Num4, args.Num6, hostname)

	var v4list, v6list []net.IPNet

	if args.Num4 != 0 {
		// Assign IPv4 addresses.
		log.Debugf("Assigning IPv4 addresses")
		for _, pool := range args.IPv4Pools {
			if pool.IP.To4() == nil {
				return nil, nil, fmt.Errorf("provided IPv4 IPPools list contains one or more IPv6 IPPools")
			}
		}
		v4list, err = c.autoAssign(ctx, args.Num4, args.HandleID, args.Attrs, args.IPv4Pools, args.ID, 4, hostname, args.MaxBlocksPerHost, args.HostReservedAttrIPv4s)
		if err != nil {
			log.Errorf("Error assigning IPV4 addresses: %v", err)
			return v4list, nil, err
		}
	}

	if args.Num6 != 0 {
		// If no err assigning V4, try to assign any V6.
		log.Debugf("Assigning IPv6 addresses")
		for _, pool := range args.IPv6Pools {
			if pool.IP.To4() != nil {
				return nil, nil, fmt.Errorf("provided IPv6 IPPools list contains one or more IPv4 IPPools")
			}
		}
		v6list, err = c.autoAssign(ctx, args.Num6, args.HandleID, args.Attrs, args.IPv6Pools, args.ID, 6, hostname, args.MaxBlocksPerHost, args.HostReservedAttrIPv6s)
		if err != nil {
			log.Errorf("Error assigning IPV6 addresses: %v", err)
			return v4list, v6list, err
		}
	}

	return v4list, v6list, nil
}

// getBlockFromAffinity returns the block referenced by the given affinity, attempting to create it if
// it does not exist. getBlockFromAffinity will delete the provided affinity if it does not match the actual
// affinity of the block.
func (c ipamClient) getBlockFromAffinity(ctx context.Context, aff *model.KVPair, rsvdAttr *HostReservedAttr) (*model.KVPair, error) {
	// Parse out affinity data.
	cidr := aff.Key.(model.BlockAffinityKey).CIDR
	host := aff.Key.(model.BlockAffinityKey).Host
	id := aff.Key.(model.BlockAffinityKey).ID
	state := aff.Value.(*model.BlockAffinity).State
	//Kubesphere added file id
	logCtx := log.WithFields(log.Fields{"host": host, "id": id, "cidr": cidr})

	// Get the block referenced by this affinity.
	logCtx.Info("Attempting to load block")
	b, err := c.blockReaderWriter.queryBlock(ctx, id, cidr, "")
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
			// The block referenced by the affinity doesn't exist. Try to create it.
			logCtx.Info("The referenced block doesn't exist, trying to create it")
			aff.Value.(*model.BlockAffinity).State = model.StatePending
			aff, err = c.blockReaderWriter.updateAffinity(ctx, aff)
			if err != nil {
				logCtx.WithError(err).Warn("Error updating block affinity")
				return nil, err
			}
			logCtx.Info("Wrote affinity as pending")

			cfg, err := c.GetIPAMConfig(ctx)
			if err != nil {
				logCtx.WithError(err).Errorf("Error getting IPAM Config")
				return nil, err
			}

			// Claim the block, which will also confirm the affinity.
			logCtx.Info("Attempting to claim the block")
			b, err := c.blockReaderWriter.claimAffineBlock(ctx, aff, *cfg, rsvdAttr)
			if err != nil {
				logCtx.WithError(err).Warn("Error claiming block")
				return nil, err
			}
			return b, nil
		}
		logCtx.WithError(err).Error("Error getting block")
		return nil, err
	}

	// If the block doesn't match the affinity, it means we've got a stale affininty hanging around.
	// We should remove it.
	blockAffinity := b.Value.(*model.AllocationBlock).Affinity
	if blockAffinity == nil || *blockAffinity != fmt.Sprintf("host:%s", host) {
		logCtx.WithField("blockAffinity", blockAffinity).Warn("Block does not match the provided affinity, deleting stale affinity")
		err := c.blockReaderWriter.deleteAffinity(ctx, aff)
		if err != nil {
			logCtx.WithError(err).Warn("Error deleting stale affinity")
			return nil, err
		}
		return nil, errStaleAffinity(fmt.Sprintf("Affinity is stale: %+v", aff))
	}

	// If the block does match the affinity but the affinity has not been confirmed,
	// try to confirm it. Treat empty string as confirmed for compatibility with older data.
	if state != model.StateConfirmed && state != "" {
		// Write the affinity as pending.
		logCtx.Info("Affinity has not been confirmed - attempt to confirm it")
		aff.Value.(*model.BlockAffinity).State = model.StatePending
		aff, err = c.blockReaderWriter.updateAffinity(ctx, aff)
		if err != nil {
			logCtx.WithError(err).Warn("Error marking affinity as pending as part of confirmation process")
			return nil, err
		}

		// CAS the block to get a new revision and invalidate any other instances
		// that might be trying to operate on the block.
		logCtx.Info("Writing block to get a new revision")
		b, err = c.blockReaderWriter.updateBlock(ctx, b)
		if err != nil {
			logCtx.WithError(err).Debug("Error writing block")
			return nil, err
		}

		// Confirm the affinity.
		logCtx.Info("Attempting to confirm affinity")
		aff.Value.(*model.BlockAffinity).State = model.StateConfirmed
		aff, err = c.blockReaderWriter.updateAffinity(ctx, aff)
		if err != nil {
			logCtx.WithError(err).Debug("Error confirming affinity")
			return nil, err
		}
		logCtx.Info("Affinity confirmed successfully")
	}
	logCtx.Info("Affinity is confirmed and block has been loaded")
	return b, nil
}

// function to check if context has associated OS parameter
// fallback to GOOS in other case
func detectOS(ctx context.Context) string {
	if osOverride := ctx.Value("windowsHost"); osOverride != nil {
		return osOverride.(string)
	}
	return runtime.GOOS
}

// Kubesphere changed
// determinePools compares a list of requested pools with the enabled pools and returns the intersect.
// If any requested pool does not exist, or is not enabled, an error is returned.
// If no pools are requested, all enabled pools are returned.
// Also applies selector logic on node labels to determine if the pool is a match.
// Returns the set of matching pools as well as the full set of ip pools.
func (c ipamClient) determinePools(ctx context.Context, requestedPoolNets []net.IPNet, id uint32, version int, node v1alpha1.Node, maxPrefixLen int) (matchingPools, enabledPools []v1alpha1.IPPool, err error) {
	// Get all the enabled IP pools from the datastore.
	tmpPools, err := c.pools.GetEnabledPools(version)
	if err != nil {
		log.WithError(err).Errorf("Error getting IP pools")
		return
	}

	for _, p := range tmpPools {
		if p.ID() == id {
			enabledPools = append(enabledPools, p)
		}
	}

	log.Debugf("enabled pools: %v", enabledPools)
	log.Debugf("requested pools: %v", requestedPoolNets)

	// Build a map so we can lookup existing pools.
	pm := map[string]v1alpha1.IPPool{}

	for _, p := range enabledPools {
		if p.Spec.BlockSize > maxPrefixLen {
			log.Warningf("skipping pool %v due to blockSize %d bigger than %d", p, p.Spec.BlockSize, maxPrefixLen)
			continue
		}
		pm[p.Spec.CIDR] = p
	}

	if len(pm) == 0 {
		// None of the enabled pools are qualified.
		err = ErrNoQualifiedPool
		return
	}

	// Build a list of requested IP pool objects based on the provided CIDRs, validating
	// that each one actually exists and is enabled for IPAM.
	requestedPools := []v1alpha1.IPPool{}
	for _, rp := range requestedPoolNets {
		if pool, ok := pm[rp.String()]; !ok {
			// The requested pool doesn't exist.
			err = fmt.Errorf("the given pool (%s) does not exist, or is not enabled", rp.IPNet.String())
			return
		} else {
			requestedPools = append(requestedPools, pool)
		}
	}

	// If requested IP pools are provided, use those unconditionally. We will ignore
	// IP pool selectors in this case. We need this for backwards compatibility, since IP pool
	// node selectors have not always existed.
	if len(requestedPools) > 0 {
		log.Debugf("Using the requested IP pools")
		matchingPools = requestedPools
		return
	}

	// At this point, we've determined the set of enabled IP pools which are valid for use.
	// We only want to use IP pools which actually match this node, so do a filter based on
	// selector.
	for _, pool := range enabledPools {
		/*
			var matches bool
			matches, err = pool.SelectsNode(node)
			if err != nil {
				log.WithError(err).WithField("pool", pool).Error("failed to determine if node matches pool")
				return
			}
			if !matches {
				// Do not consider pool enabled if the nodeSelector doesn't match the node's labels.
				log.Debugf("IP pool does not match this node: %s", pool.Name)
				continue
			}
			log.Debugf("IP pool matches this node: %s", pool.Name)
		*/
		matchingPools = append(matchingPools, pool)
	}

	return
}

// Kubesphere changed
// prepareAffinityBlocksForHost returns a list of blocks affine to a node based on requested ippools.
// It also release any emptied blocks still affine to this host but no longer part of an IP Pool which
// selects this node. It returns matching pools, list of host-affine blocks and any error encountered.
func (c ipamClient) prepareAffinityBlocksForHost(
	ctx context.Context,
	requestedPools []net.IPNet,
	id uint32,
	version int,
	host string,
	rsvdAttr *HostReservedAttr) ([]v1alpha1.IPPool, []net.IPNet, error) {

	/*
		// Retrieve node for given hostname to use for ip pool node selection
		node, err := c.client.Get(ctx, model.ResourceKey{Kind: v1alpha1.KindNode, Name: host}, "")
		if err != nil {
			log.WithError(err).WithField("node", host).Error("failed to get node for host")
			return nil, nil, err
		}

		// Make sure the returned value is OK.
		v1alpha1n, ok := node.Value.(*v1alpha1.Node)
		if !ok {
			return nil, nil, fmt.Errorf("Datastore returned malformed node object")
		}
	*/

	maxPrefixLen, err := getMaxPrefixLen(version, rsvdAttr)
	if err != nil {
		return nil, nil, err
	}

	// Determine the correct set of IP pools to use for this request.
	pools, allPools, err := c.determinePools(ctx, requestedPools, id, version, v1alpha1.Node{}, maxPrefixLen)
	if err != nil {
		return nil, nil, err
	}

	// If there are no pools, we cannot assign addresses.
	if len(pools) == 0 {
		return nil, nil, fmt.Errorf("no configured Calico pools for node %v", host)
	}

	logCtx := log.WithFields(log.Fields{"host": host})

	logCtx.Info("Looking up existing affinities for host")
	affBlocks, affBlocksToRelease, err := c.blockReaderWriter.getAffineBlocks(ctx, host, version, id,  pools)
	if err != nil {
		return nil, nil, err
	}

	// Release any emptied blocks still affine to this host but no longer part of an IP Pool which selects this node.
	for _, block := range affBlocksToRelease {
		// Determine the pool for each block.
		pool, err := c.blockReaderWriter.getPoolForIP(id, net.IP{block.IP}, allPools)
		if err != nil {
			log.WithError(err).Warnf("Failed to get pool for IP")
			continue
		}
		if pool == nil {
			logCtx.WithFields(log.Fields{"block": block}).Warn("No pool found for block, skipping")
			continue
		}

		/*
			// Determine if the pool selects the current node, refusing to release this particular block affinity if so.
			blockSelectsNode, err := pool.SelectsNode(*v1alpha1n)
			if err != nil {
				logCtx.WithError(err).WithField("pool", pool).Error("Failed to determine if node matches pool, skipping")
				continue
			}
			if blockSelectsNode {
				logCtx.WithFields(log.Fields{"pool": pool, "block": block}).Debug("Block's pool still selects node, refusing to remove affinity")
				continue
			}
		*/

		// Release the block affinity, requiring it to be empty.
		for i := 0; i < datastoreRetries; i++ {
			if err = c.blockReaderWriter.releaseBlockAffinity(ctx, host, id, block, true); err != nil {
				if _, ok := err.(errBlockClaimConflict); ok {
					// Not claimed by this host - ignore.
				} else if _, ok := err.(errBlockNotEmpty); ok {
					// Block isn't empty - ignore.
				} else if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
					// Block does not exist - ignore.
				} else {
					logCtx.WithError(err).WithField("block", block).Warn("Error occurred releasing block, trying again")
					continue
				}
			}
			logCtx.WithField("block", block).Info("Released affine block that no longer selects this host")
			break
		}
	}

	return pools, affBlocks, nil
}

// blockAssignState manages the state in relation to the request of finding or claiming a block for a host.
type blockAssignState struct {
	client                ipamClient
	version               int
	host                  string
	id                    uint32
	pools                 []v1alpha1.IPPool
	remainingAffineBlocks []net.IPNet
	hostReservedAttr      *HostReservedAttr
	allowNewClaim         bool

	// For UT purpose, how many times datastore retry has been triggered.
	datastoreRetryCount int
}

// Given a list of host-affine blocks, findOrClaimBlock returns one block with minimum free ips.
// It tries to use one of the current host-affine blocks first and if not found, it will claim a new block
// and assign affinity.
// It returns a block, a boolean if block is newly claimed and any error encountered.
func (s *blockAssignState) findOrClaimBlock(ctx context.Context, minFreeIps int) (*model.KVPair, bool, error) {
	logCtx := log.WithFields(log.Fields{"host": s.host})

	// First, we try to find a block from one of the existing host-affine blocks.
	for len(s.remainingAffineBlocks) > 0 {
		// Pop first cidr.
		cidr := s.remainingAffineBlocks[0]
		s.remainingAffineBlocks = s.remainingAffineBlocks[1:]

		// Checking this block - if we hit a CAS error, we'll try this block again.
		// For any other error, we'll break out and try the next affine block.
		for i := 0; i < datastoreRetries; i++ {
			// Get the affinity.
			logCtx.Infof("Trying affinity for %s", cidr)
			aff, err := s.client.blockReaderWriter.queryAffinity(ctx, s.host, s.id, cidr, "")
			if err != nil {
				logCtx.WithError(err).Warnf("Error getting affinity")
				break
			}

			// Get the block which is referenced by the affinity, creating it if necessary.
			b, err := s.client.getBlockFromAffinity(ctx, aff, s.hostReservedAttr)
			if err != nil {
				// Couldn't get a block for this affinity.
				if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
					logCtx.WithError(err).Debug("CAS error getting affine block - retry")
					continue
				}
				logCtx.WithError(err).Warn("Couldn't get block for affinity, try next one")
				break
			}

			// Pull out the block.
			block := allocationBlock{b.Value.(*model.AllocationBlock)}
			if block.NumFreeAddresses() >= minFreeIps {
				logCtx.Debugf("Block '%s' has %d free ips which is more than %d ips required.", cidr.String(), block.NumFreeAddresses(), minFreeIps)
				return b, false, nil
			} else {
				logCtx.Debugf("Block '%s' has %d free ips which is less than %d ips required.", cidr.String(), block.NumFreeAddresses(), minFreeIps)
				break
			}
		}
	}

	logCtx.Infof("Ran out of existing affine blocks for host")

	if !s.allowNewClaim {
		return nil, false, ErrBlockLimit
	}

	// Find unclaimed block if AutoAllocateBlocks is true.
	config, err := s.client.GetIPAMConfig(ctx)
	if err != nil {
		return nil, false, err
	}
	logCtx.Debugf("Allocate new blocks? Config: %+v", config)
	if config.AutoAllocateBlocks {
		for i := 0; i < datastoreRetries; i++ {
			// Claim a new block.
			logCtx.Infof("No more affine blocks, but need to claim more block -- allocate another block")

			// First, try to find an unclaimed block.
			logCtx.Info("Looking for an unclaimed block")
			subnet, err := s.client.blockReaderWriter.findUnclaimedBlock(ctx, s.host, s.version, s.pools, *config)
			if err != nil {
				if _, ok := err.(noFreeBlocksError); ok {
					// No free blocks.  Break.
					logCtx.Info("No free blocks available for allocation")
				} else {
					log.WithError(err).Error("Failed to find an unclaimed block")
				}
				return nil, false, err
			}
			logCtx := log.WithFields(log.Fields{"host": s.host, "subnet": subnet})
			logCtx.Info("Found unclaimed block")

			for j := 0; j < datastoreRetries; j++ {
				// We found an unclaimed block - claim affinity for it.
				pa, err := s.client.blockReaderWriter.getPendingAffinity(ctx, s.host, s.id, *subnet)
				if err != nil {
					if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
						logCtx.WithError(err).Debug("CAS error claiming pending affinity, retry")
						continue
					}
					logCtx.WithError(err).Errorf("Error claiming pending affinity")
					return nil, false, err
				}

				// We have an affinity - try to get the block.
				b, err := s.client.getBlockFromAffinity(ctx, pa, s.hostReservedAttr)
				if err != nil {
					if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
						logCtx.WithError(err).Debug("CAS error getting block, retry")
						continue
					} else if _, ok := err.(errBlockClaimConflict); ok {
						logCtx.WithError(err).Debug("Block taken by someone else, find a new one")
						break
					} else if _, ok := err.(errStaleAffinity); ok {
						logCtx.WithError(err).Debug("Affinity is stale, find a new one")
						break
					}
					logCtx.WithError(err).Errorf("Error getting block for affinity")
					return nil, false, err
				}

				// Claim successful.
				block := allocationBlock{b.Value.(*model.AllocationBlock)}
				if block.NumFreeAddresses() >= minFreeIps {
					logCtx.Infof("Block '%s' has %d free ips which is more than %d ips required.", b.Key.(model.BlockKey).CIDR, block.NumFreeAddresses(), minFreeIps)
					return b, true, nil
				} else {
					errString := fmt.Sprintf("Block '%s' has %d free ips which is less than %d ips required.", b.Key.(model.BlockKey).CIDR, block.NumFreeAddresses(), minFreeIps)
					logCtx.Errorf(errString)
					return nil, false, errors.New(errString)
				}
				s.datastoreRetryCount++
			}
			s.datastoreRetryCount++
		}
		return nil, false, errors.New("Max retries hit - excessive concurrent IPAM requests")
	}

	return nil, false, errors.New("failed to find or claim a block")
}

func (c ipamClient) autoAssign(ctx context.Context, num int, handleID *string, attrs map[string]string, requestedPools []net.IPNet, id uint32, version int, host string, maxNumBlocks int, rsvdAttr *HostReservedAttr) ([]net.IPNet, error) {

	// First, get the existing host-affine blocks.
	logCtx := log.WithFields(log.Fields{"host": host})
	if handleID != nil {
		logCtx = logCtx.WithField("handle", *handleID)
	}
	logCtx.Info("Looking up existing affinities for host")
	pools, affBlocks, err := c.prepareAffinityBlocksForHost(ctx, requestedPools, id, version, host, rsvdAttr)
	if err != nil {
		return nil, err
	}

	logCtx.Debugf("Found %d affine IPv%d blocks for host: %v", len(affBlocks), version, affBlocks)
	ips := []net.IPNet{}

	// Record how many blocks we own so we can check against the limit later.
	numBlocksOwned := len(affBlocks)

	config, err := c.GetIPAMConfig(ctx)
	if err != nil {
		return nil, err
	}

	// Merge in any global config, if it exists. We use the more restrictive value between
	// the global max block limit, and the limit provided on this particular request.
	if config.MaxBlocksPerHost > 0 && maxNumBlocks > 0 && maxNumBlocks > config.MaxBlocksPerHost {
		// The global config is more restrictive, so use it instead.
		maxNumBlocks = config.MaxBlocksPerHost
	} else if maxNumBlocks == 0 {
		// No per-request value, so use the global one.
		maxNumBlocks = config.MaxBlocksPerHost
	}
	if maxNumBlocks > 0 {
		logCtx.Debugf("Host must not use more than %d blocks", maxNumBlocks)
	}

	s := &blockAssignState{
		client:                c,
		version:               version,
		host:                  host,
		pools:                 pools,
		id:                    id,
		remainingAffineBlocks: affBlocks,
		hostReservedAttr:      rsvdAttr,
		allowNewClaim:         true,
	}

	// Allocate the IPs.
	for len(ips) < num {
		var b *model.KVPair

		rem := num - len(ips)
		if maxNumBlocks > 0 && numBlocksOwned >= maxNumBlocks {
			s.allowNewClaim = false
		}

		b, newlyClaimed, err := s.findOrClaimBlock(ctx, 1)
		if err != nil {
			if _, ok := err.(noFreeBlocksError); ok {
				// Skip to check non-affine blocks
				break
			}
			if errors.Is(err, ErrBlockLimit) {
				log.Warnf("Unable to allocate a new IPAM block; host already has %v blocks but "+
					"blocks per host limit is %v", numBlocksOwned, maxNumBlocks)
				return ips, ErrBlockLimit
			}
			return nil, err
		}

		if newlyClaimed {
			numBlocksOwned++
		}

		// We have got a block b.
		for i := 0; i < datastoreRetries; i++ {
			newIPs, err := c.assignFromExistingBlock(ctx, b, rem, handleID, attrs, host, config.StrictAffinity)
			if err != nil {
				if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
					log.WithError(err).Debug("CAS Error assigning from new block - retry")

					// At this point, block b's Unallocated field has been reduced already.
					// We should get the original block from datastore again.
					blockCIDR := b.Key.(model.BlockKey).CIDR
					b, err = c.blockReaderWriter.queryBlock(ctx, s.id, blockCIDR, "")
					if err != nil {
						logCtx.WithError(err).Warn("Failed to get block again after update conflict")
						break
					}

					// Block b is in sync with datastore. Retry assigning IP.
					continue
				}
				logCtx.WithError(err).Warningf("Failed to assign IPs in newly allocated block")
				break
			}
			logCtx.Debugf("Assigned IPs from new block: %s", newIPs)
			ips = append(ips, newIPs...)
			rem = num - len(ips)
			break
		}
	}

	// If there are still addresses to allocate, we've now tried all blocks
	// with some affinity to us, and tried (and failed) to allocate new
	// ones.  If we do not require strict host affinity, our last option is
	// a random hunt through any blocks we haven't yet tried.
	//
	// Note that this processing simply takes all of the IP pools and breaks
	// them up into block-sized CIDRs, then shuffles and searches through each
	// CIDR.  This algorithm does not work if we disallow auto-allocation of
	// blocks because the allocated blocks may be sparsely populated in the
	// pools resulting in a very slow search for free addresses.
	//
	// If we need to support non-strict affinity and no auto-allocation of
	// blocks, then we should query the actual allocation blocks and assign
	// from those.
	rem := num - len(ips)
	if config.StrictAffinity != true && rem != 0 {
		logCtx.Infof("Attempting to assign %d more addresses from non-affine blocks", rem)

		// Iterate over pools and assign addresses until we either run out of pools,
		// or the request has been satisfied.
		logCtx.Info("Looking for blocks with free IP addresses")
		for _, p := range pools {
			logCtx.Debugf("Assigning from non-affine blocks in pool %s", p.Spec.CIDR)
			newBlockCIDR := randomBlockGenerator(p, host)
			for rem > 0 {
				// Grab a new random block.
				blockCIDR := newBlockCIDR()
				if blockCIDR == nil {
					logCtx.Warningf("All addresses exhausted in pool %s", p.Spec.CIDR)
					break
				}

				for i := 0; i < datastoreRetries; i++ {
					b, err := c.blockReaderWriter.queryBlock(ctx, id, *blockCIDR, "")
					if err != nil {
						logCtx.WithError(err).Warn("Failed to get non-affine block")
						break
					}

					// Attempt to assign from the block.
					logCtx.Infof("Attempting to assign IPs from non-affine block %s", blockCIDR.String())
					newIPs, err := c.assignFromExistingBlock(ctx, b, rem, handleID, attrs, host, false)
					if err != nil {
						if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
							logCtx.WithError(err).Debug("CAS error assigning from non-affine block - retry")
							continue
						}
						logCtx.WithError(err).Warningf("Failed to assign IPs from non-affine block in pool %s", p.Spec.CIDR)
						break
					}
					if len(newIPs) == 0 {
						break
					}
					logCtx.Infof("Successfully assigned IPs from non-affine block %s", blockCIDR.String())
					ips = append(ips, newIPs...)
					rem = num - len(ips)
					break
				}
			}
		}
	}

	logCtx.Infof("Auto-assigned %d out of %d IPv%ds: %v", len(ips), num, version, ips)
	return ips, nil
}

// AssignIP assigns the provided IP address to the provided host.  The IP address
// must fall within a configured pool.  AssignIP will claim block affinity as needed
// in order to satisfy the assignment.  An error will be returned if the IP address
// is already assigned, or if StrictAffinity is enabled and the address is within
// a block that does not have affinity for the given host.
func (c ipamClient) AssignIP(ctx context.Context, args AssignIPArgs) error {
	hostname, err := decideHostname(args.Hostname)
	if err != nil {
		return err
	}
	log.Infof("Assigning IP %s to host: %s", args.IP, hostname)

	pool, err := c.blockReaderWriter.getPoolForIP(args.ID, args.IP, nil)
	if err != nil {
		return err
	}
	if pool == nil {
		return errors.New("The provided IP address is not in a configured pool\n")
	}

	cfg, err := c.GetIPAMConfig(ctx)
	if err != nil {
		log.Errorf("Error getting IPAM Config: %v", err)
		return err
	}

	blockCIDR := getBlockCIDRForAddress(args.IP, pool)
	log.Debugf("IP %s is in block '%s'", args.IP.String(), blockCIDR.String())
	for i := 0; i < datastoreRetries; i++ {
		obj, err := c.blockReaderWriter.queryBlock(ctx, args.ID, blockCIDR, "")
		if err != nil {
			if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
				log.WithError(err).Error("Error getting block")
				return err
			}

			log.Debugf("Block for IP %s does not yet exist, creating", args.IP)
			cfg, err = c.GetIPAMConfig(ctx)
			if err != nil {
				log.Errorf("Error getting IPAM Config: %v", err)
				return err
			}

			pa, err := c.blockReaderWriter.getPendingAffinity(ctx, hostname, args.ID, blockCIDR)
			if err != nil {
				if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
					log.WithError(err).Debug("CAS error claiming affinity for block - retry")
					continue
				}
				return err
			}

			obj, err = c.blockReaderWriter.claimAffineBlock(ctx, pa, *cfg, args.HostReservedAttr)
			if err != nil {
				if _, ok := err.(*errBlockClaimConflict); ok {
					log.Warningf("Someone else claimed block %s before us", blockCIDR.String())
					continue
				} else if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
					log.WithError(err).Debug("CAS error claiming affine block - retry")
					continue
				}
				log.WithError(err).Error("Error claiming block")
				return err
			}
			log.Infof("Claimed new block: %s", blockCIDR)
		}

		block := allocationBlock{obj.Value.(*model.AllocationBlock)}
		err = block.assign(cfg.StrictAffinity, args.IP, args.HandleID, args.Attrs, hostname)
		if err != nil {
			log.Errorf("Failed to assign address %v: %v", args.IP, err)
			return err
		}

		// Increment handle.
		if args.HandleID != nil {
			c.incrementHandle(ctx, *args.HandleID, args.ID, blockCIDR, 1)
		}

		// Update the block using the original KVPair to do a CAS.  No need to
		// update the Value since we have been manipulating the Value pointed to
		// in the KVPair.
		_, err = c.blockReaderWriter.updateBlock(ctx, obj)
		if err != nil {
			if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
				log.WithError(err).Debug("CAS error assigning IP - retry")
				continue
			}

			log.WithError(err).Warningf("Update failed on block %s", block.CIDR.String())
			if args.HandleID != nil {
				if err := c.decrementHandle(ctx, *args.HandleID, args.ID, blockCIDR, 1); err != nil {
					log.WithError(err).Warn("Failed to decrement handle")
				}
			}
			return err
		}
		return nil
	}
	return errors.New("Max retries hit - excessive concurrent IPAM requests")
}

// ReleaseIPs releases any of the given IP addresses that are currently assigned,
// so that they are available to be used in another assignment.
func (c ipamClient) ReleaseIPs(ctx context.Context, id uint32, ips []net.IP) ([]net.IP, error) {
	log.Infof("Releasing IP addresses: %v", ips)
	unallocated := []net.IP{}

	// Group IP addresses by block to minimize the number of writes
	// to the datastore required to release the given addresses.
	ipsByBlock := map[string][]net.IP{}
	for _, ip := range ips {
		var cidrStr string

		pool, err := c.blockReaderWriter.getPoolForIP(id, ip, nil)
		if err != nil {
			log.WithError(err).Warnf("Failed to get pool for IP")
			return nil, err
		}
		if pool == nil {
			if cidr, err := c.blockReaderWriter.getBlockForIP(ctx, id, ip); err != nil {
				return nil, err
			} else {
				if cidr == nil {
					// The IP isn't in any block so it's already unallocated.
					unallocated = append(unallocated, ip)

					// Move on to the next IP
					continue
				}
				cidrStr = cidr.String()
			}
		} else {
			cidrStr = getBlockCIDRForAddress(ip, pool).String()
		}

		// Check if we've already got an entry for this block.
		if _, exists := ipsByBlock[cidrStr]; !exists {
			// Entry does not exist, create it.
			ipsByBlock[cidrStr] = []net.IP{}
		}

		// Append to the list.
		ipsByBlock[cidrStr] = append(ipsByBlock[cidrStr], ip)
	}

	// Release IPs for each block.
	for cidrStr, ips := range ipsByBlock {
		_, cidr, _ := net.ParseCIDR(cidrStr)
		unalloc, err := c.releaseIPsFromBlock(ctx, id, ips, *cidr)
		if err != nil {
			log.Errorf("Error releasing IPs: %v", err)
			return nil, err
		}
		unallocated = append(unallocated, unalloc...)
	}
	return unallocated, nil
}

func (c ipamClient) releaseIPsFromBlock(ctx context.Context, id uint32, ips []net.IP, blockCIDR net.IPNet) ([]net.IP, error) {
	logCtx := log.WithField("cidr", blockCIDR)
	for i := 0; i < datastoreRetries; i++ {
		logCtx.Info("Getting block so we can release IPs")

		// Get allocation block for cidr.
		obj, err := c.blockReaderWriter.queryBlock(ctx, id, blockCIDR, "")
		if err != nil {
			if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
				// The block does not exist - all addresses must be unassigned.
				return ips, nil
			} else {
				// Unexpected error reading block.
				return nil, err
			}
		}

		// Release the IPs.
		b := allocationBlock{obj.Value.(*model.AllocationBlock)}
		unallocated, handles, err2 := b.release(ips)
		if err2 != nil {
			return nil, err2
		}
		if len(ips) == len(unallocated) {
			// All the given IP addresses are already unallocated.
			// Just return.
			logCtx.Info("No IPs need to be released")
			return unallocated, nil
		}

		// If the block is empty and has no affinity, we can delete it.
		// Otherwise, update the block using CAS.  There is no need to update
		// the Value since we have updated the structure pointed to in the
		// KVPair.
		var updateErr error
		if b.empty() && b.Affinity == nil {
			logCtx.Info("Deleting non-affine block")
			updateErr = c.blockReaderWriter.deleteBlock(ctx, obj)
		} else {
			logCtx.Info("Updating assignments in block")
			_, updateErr = c.blockReaderWriter.updateBlock(ctx, obj)
		}

		if updateErr != nil {
			if _, ok := updateErr.(cerrors.ErrorResourceUpdateConflict); ok {
				// Comparison error - retry.
				logCtx.Warningf("Failed to update block - retry #%d", i)
				continue
			} else {
				// Something else - return the error.
				logCtx.WithError(updateErr).Errorf("Error updating block")
				return nil, updateErr
			}
		}

		// Success - decrement handles.
		logCtx.Debugf("Decrementing handles: %v", handles)
		for handleID, amount := range handles {
			if err := c.decrementHandle(ctx, handleID, id, blockCIDR, amount); err != nil {
				logCtx.WithError(err).Warn("Failed to decrement handle")
			}
		}

		/*
			// Determine whether or not the block's pool still matches the node.
			if err := c.ensureConsistentAffinity(ctx, obj.Value.(*model.AllocationBlock)); err != nil {
				logCtx.WithError(err).Warn("Error ensuring consistent affinity but IP already released. Returning no error.")
			}
		*/
		return unallocated, nil
	}
	return nil, errors.New("Max retries hit - excessive concurrent IPAM requests")
}

func (c ipamClient) assignFromExistingBlock(ctx context.Context, block *model.KVPair, num int, handleID *string, attrs map[string]string, host string, affCheck bool) ([]net.IPNet, error) {
	blockCIDR := block.Key.(model.BlockKey).CIDR
	id := block.Key.(model.BlockKey).ID
	logCtx := log.WithFields(log.Fields{"host": host, "block": blockCIDR})
	if handleID != nil {
		logCtx = logCtx.WithField("handle", *handleID)
	}
	logCtx.Infof("Attempting to assign %d addresses from block", num)

	// Pull out the block.
	b := allocationBlock{block.Value.(*model.AllocationBlock)}

	ips, err := b.autoAssign(num, handleID, host, attrs, affCheck)
	if err != nil {
		logCtx.WithError(err).Errorf("Error in auto assign")
		return nil, err
	}
	if len(ips) == 0 {
		logCtx.Infof("Block is full")
		return []net.IPNet{}, nil
	}

	// Increment handle count.
	if handleID != nil {
		logCtx.Debug("Incrementing handle")
		c.incrementHandle(ctx, *handleID, id, blockCIDR, num)
	}

	// Update the block using CAS by passing back the original
	// KVPair.
	logCtx.Info("Writing block in order to claim IPs")
	block.Value = b.AllocationBlock
	_, err = c.blockReaderWriter.updateBlock(ctx, block)
	if err != nil {
		logCtx.WithError(err).Infof("Failed to update block")
		if handleID != nil {
			logCtx.Debug("Decrementing handle since we failed to allocate IP(s)")
			if err := c.decrementHandle(ctx, *handleID, id, blockCIDR, num); err != nil {
				logCtx.WithError(err).Warnf("Failed to decrement handle")
			}
		}
		return nil, err
	}
	logCtx.Infof("Successfully claimed IPs: %v", ips)
	return ips, nil
}

func (c ipamClient) hostBlockPairs(ctx context.Context, id uint32, pool net.IPNet) (map[string]string, error) {
	pairs := map[string]string{}

	// Get all blocks and their affinities.
	objs, err := c.client.List(ctx, model.BlockAffinityListOptions{}, "")
	if err != nil {
		log.Errorf("Error querying block affinities: %v", err)
		return nil, err
	}

	// Iterate through each block affinity and build up a mapping
	// of blockCidr -> host.
	log.Debugf("Getting block -> host mappings")
	for _, o := range objs.KVPairs {
		k := o.Key.(model.BlockAffinityKey)

		// Only add the pair to the map if the block belongs to the pool.
		if pool.Contains(k.CIDR.IPNet.IP) && id == k.ID {
			pairs[k.CIDR.String()] = k.Host
		}
		log.Debugf("Block %s -> %s", k.CIDR.String(), k.Host)
	}

	return pairs, nil
}

// ReleasePoolAffinities releases affinity for all blocks within
// the specified pool across all hosts.
func (c ipamClient) ReleasePoolAffinities(ctx context.Context, id uint32, pool net.IPNet) error {
	log.Infof("Releasing block affinities within pool '%s'", pool.String())
	for i := 0; i < ipamKeyErrRetries; i++ {
		retry := false
		pairs, err := c.hostBlockPairs(ctx, id, pool)
		if err != nil {
			return err
		}

		if len(pairs) == 0 {
			log.Debugf("No blocks have affinity")
			return nil
		}

		for blockString, host := range pairs {
			_, blockCIDR, _ := net.ParseCIDR(blockString)
			logCtx := log.WithField("cidr", blockCIDR)
			for i := 0; i < datastoreRetries; i++ {
				err = c.blockReaderWriter.releaseBlockAffinity(ctx, host, id, *blockCIDR, false)
				if err != nil {
					if _, ok := err.(errBlockClaimConflict); ok {
						retry = true
					} else if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
						logCtx.Debugf("No such block")
						break
					} else if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
						logCtx.WithError(err).Debug("CAS error releasing block affinity - retry")
						continue
					} else {
						logCtx.WithError(err).Errorf("Error releasing affinity")
						return err
					}
				}
				break
			}
		}

		if !retry {
			return nil
		}
	}
	return errors.New("Max retries hit - excessive concurrent IPAM requests")
}

func parseHandleBlockKey(k string) (uint32, *net.IPNet) {
	tmp := strings.SplitN(k, "-", 2)
	id, _ := strconv.Atoi(tmp[0])
	_, blockCIDR, _ := net.ParseCIDR(tmp[1])

	return uint32(id), blockCIDR
}

// IpsByHandle returns a list of all IP addresses that have been
// assigned using the provided handle.
func (c ipamClient) IPsByHandle(ctx context.Context, handleID string) ([]net.IP, error) {
	obj, err := c.blockReaderWriter.queryHandle(ctx, handleID, "")
	if err != nil {
		return nil, err
	}
	handle := allocationHandle{obj.Value.(*model.IPAMHandle)}

	assignments := []net.IP{}
	for k, _ := range handle.Block {
		id, blockCIDR := parseHandleBlockKey(k)
		obj, err := c.blockReaderWriter.queryBlock(ctx, uint32(id), *blockCIDR, "")
		if err != nil {
			log.WithError(err).Warningf("Couldn't read block %s referenced by handle %s", blockCIDR, handleID)
			continue
		}

		// Pull out the allocationBlock and get all the assignments from it.
		b := allocationBlock{obj.Value.(*model.AllocationBlock)}
		assignments = append(assignments, b.ipsByHandle(handleID)...)
	}
	return assignments, nil
}

// ReleaseByHandle releases all IP addresses that have been assigned
// using the provided handle.
func (c ipamClient) ReleaseByHandle(ctx context.Context, handleID string) error {
	log.Infof("Releasing all IPs with handle '%s'", handleID)
	obj, err := c.blockReaderWriter.queryHandle(ctx, handleID, "")
	if err != nil {
		return err
	}
	handle := allocationHandle{obj.Value.(*model.IPAMHandle)}

	for blockStr, _ := range handle.Block {
		id, blockCIDR := parseHandleBlockKey(blockStr)
		if err := c.releaseByHandle(ctx, handleID, id, *blockCIDR); err != nil {
			return err
		}
	}
	return nil
}

func (c ipamClient) releaseByHandle(ctx context.Context, handleID string, id uint32, blockCIDR net.IPNet) error {
	logCtx := log.WithFields(log.Fields{"handle": handleID, "cidr": blockCIDR})
	for i := 0; i < datastoreRetries; i++ {
		logCtx.Debug("Querying block so we can release IPs by handle")
		obj, err := c.blockReaderWriter.queryBlock(ctx, id, blockCIDR, "")
		if err != nil {
			if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
				// Block doesn't exist, so all addresses are already
				// unallocated.  This can happen when a handle is
				// overestimating the number of assigned addresses.
				return nil
			} else {
				return err
			}
		}

		// Release the IP by handle.
		block := allocationBlock{obj.Value.(*model.AllocationBlock)}
		num := block.releaseByHandle(handleID)
		if num == 0 {
			// Block has no addresses with this handle, so
			// all addresses are already unallocated.
			logCtx.Debug("Block has no addresses with the given handle")
			return nil
		}
		logCtx.Debugf("Block has %d IPs with the given handle", num)

		if block.empty() && block.Affinity == nil {
			logCtx.Info("Deleting block because it is now empty and has no affinity")
			err = c.blockReaderWriter.deleteBlock(ctx, obj)
			if err != nil {
				if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
					logCtx.Debug("CAD error deleting block - retry")
					continue
				}

				// Return the error unless the resource does not exist.
				if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
					logCtx.Errorf("Error deleting block: %v", err)
					return err
				}
			}
			logCtx.Info("Successfully deleted empty block")
		} else {
			// Compare and swap the AllocationBlock using the original
			// KVPair read from before.  No need to update the Value since we
			// have been directly manipulating the value referenced by the KVPair.
			logCtx.Debug("Updating block to release IPs")
			_, err = c.blockReaderWriter.updateBlock(ctx, obj)
			if err != nil {
				if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
					// Comparison failed - retry.
					logCtx.Warningf("CAS error for block, retry #%d: %v", i, err)
					continue
				} else {
					// Something else - return the error.
					logCtx.Errorf("Error updating block '%s': %v", block.CIDR.String(), err)
					return err
				}
			}
			logCtx.Debug("Successfully released IPs from block")
		}
		if err = c.decrementHandle(ctx, handleID, id, blockCIDR, num); err != nil {
			logCtx.WithError(err).Warn("Failed to decrement handle")
		}

		/*
			// Determine whether or not the block's pool still matches the node.
			if err = c.ensureConsistentAffinity(ctx, block.AllocationBlock); err != nil {
				logCtx.WithError(err).Warn("Error ensuring consistent affinity but IP already released. Returning no error.")
			}
		*/
		return nil
	}
	return errors.New("Hit max retries")
}

func (c ipamClient) incrementHandle(ctx context.Context, handleID string, id uint32, blockCIDR net.IPNet, num int) error {
	var obj *model.KVPair
	var err error
	for i := 0; i < datastoreRetries; i++ {
		obj, err = c.blockReaderWriter.queryHandle(ctx, handleID, "")
		if err != nil {
			if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
				// Handle doesn't exist - create it.
				log.Infof("Creating new handle: %s", handleID)
				bh := model.IPAMHandle{
					HandleID: handleID,
					Block:    map[string]int{},
				}
				obj = &model.KVPair{
					Key:   model.IPAMHandleKey{HandleID: handleID},
					Value: &bh,
				}
			} else {
				// Unexpected error reading handle.
				return err
			}
		}

		// Get the handle from the KVPair.
		handle := allocationHandle{obj.Value.(*model.IPAMHandle)}

		// Increment the handle for this block.
		handle.incrementBlock(id, blockCIDR, num)

		// Compare and swap the handle using the KVPair from above.  We've been
		// manipulating the structure in the KVPair, so pass straight back to
		// apply the changes.
		if obj.Revision != "" {
			// This is an existing handle - update it.
			_, err = c.blockReaderWriter.updateHandle(ctx, obj)
			if err != nil {
				log.WithError(err).Warning("Failed to update handle, retry")
				continue
			}
		} else {
			// This is a new handle - create it.
			_, err = c.client.Create(ctx, obj)
			if err != nil {
				log.WithError(err).Warning("Failed to create handle, retry")
				continue
			}
		}
		return nil
	}
	return errors.New("Max retries hit - excessive concurrent IPAM requests")

}

func (c ipamClient) decrementHandle(ctx context.Context, handleID string, id uint32, blockCIDR net.IPNet, num int) error {
	for i := 0; i < datastoreRetries; i++ {
		obj, err := c.blockReaderWriter.queryHandle(ctx, handleID, "")
		if err != nil {
			return err
		}
		handle := allocationHandle{obj.Value.(*model.IPAMHandle)}

		_, err = handle.decrementBlock(id, blockCIDR, num)
		if err != nil {
			return err
		}

		// Update / Delete as appropriate.  Since we have been manipulating the
		// data in the KVPair, just pass this straight back to the client.
		if handle.empty() {
			log.Debugf("Deleting handle: %s", handleID)
			if err = c.blockReaderWriter.deleteHandle(ctx, obj); err != nil {
				if err != nil {
					if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
						// Update conflict - retry.
						continue
					} else if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
						return err
					}
					// Already deleted.
				}
			}
		} else {
			log.Debugf("Updating handle: %s", handleID)
			if _, err = c.blockReaderWriter.updateHandle(ctx, obj); err != nil {
				if _, ok := err.(cerrors.ErrorResourceUpdateConflict); ok {
					// Update conflict - retry.
					continue
				}
				return err
			}
		}

		log.Debugf("Decremented handle '%s' by %d", handleID, num)
		return nil
	}
	return errors.New("Max retries hit - excessive concurrent IPAM requests")
}

// GetIPAMConfig returns the global IPAM configuration.  If no IPAM configuration
// has been set, returns a default configuration with StrictAffinity disabled
// and AutoAllocateBlocks enabled.
func (c ipamClient) GetIPAMConfig(ctx context.Context) (config *IPAMConfig, err error) {
	obj, err := c.client.Get(ctx, model.IPAMConfigKey{}, "")
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
			// IPAMConfig has not been explicitly set.  Return
			// a default IPAM configuration.
			config = &IPAMConfig{
				AutoAllocateBlocks: true,
				StrictAffinity:     false,
				MaxBlocksPerHost:   0,
			}
			err = nil
		} else {
			log.Errorf("Error getting IPAMConfig: %v", err)
			return nil, err
		}
	} else {
		config = c.convertBackendToIPAMConfig(obj.Value.(*model.IPAMConfig))
	}
	if detectOS(ctx) == "windows" {
		// When a Windows node owns a block, it creates a local /26 subnet object and as far as we know, it can't
		// do a longest-prefix-match between the subnet CIDR and a remote /32.  This means that we can't allow
		// remote hosts to borrow IPs from a Windows-owned block; and Windows hosts can't borrow IPs either.
		// Return error if strict affinity is not true.
		if !config.StrictAffinity {
			err = ErrStrictAffinity
			log.WithError(err).Error("Error validating ipam config")
			return nil, err
		}
	}
	return
}

func (c ipamClient) convertBackendToIPAMConfig(cfg *model.IPAMConfig) *IPAMConfig {
	return &IPAMConfig{
		StrictAffinity:     cfg.StrictAffinity,
		AutoAllocateBlocks: cfg.AutoAllocateBlocks,
		MaxBlocksPerHost:   cfg.MaxBlocksPerHost,
	}
}

func decideHostname(host string) (string, error) {
	// Determine the hostname to use - prefer the provided hostname if
	// non-nil, otherwise use the hostname reported by os.
	var hostname string
	var err error
	if host != "" {
		hostname = host
	} else {
		hostname, err = names.Hostname()
		if err != nil {
			return "", fmt.Errorf("Failed to acquire hostname: %+v", err)
		}
	}
	log.Debugf("Using hostname=%s", hostname)
	return hostname, nil
}

// GetUtilization returns IP utilization info for the specified pools, or for all pools.
func (c ipamClient) GetUtilization(ctx context.Context, args GetUtilizationArgs) ([]*PoolUtilization, error) {
	var usage []*PoolUtilization

	// Read all pools.
	allPools, err := c.pools.GetAllPools()
	if err != nil {
		log.WithError(err).Errorf("Error getting IP pools")
		return nil, err
	}

	// Identify the ones we want and create a PoolUtilization for each of those.
	wantAllPools := len(args.Pools) == 0
	wantedPools := set.FromArray(args.Pools)
	for _, pool := range allPools {
		if wantAllPools || (pool.ID() == args.ID &&
			(wantedPools.Contains(pool.Name) ||
				wantedPools.Contains(pool.Spec.CIDR))) {
			usage = append(usage, &PoolUtilization{
				Name: pool.Name,
				CIDR: net.MustParseNetwork(pool.Spec.CIDR).IPNet,
			})
		}
	}

	// If we've been asked for all pools, also report utilization for any allocation
	// blocks for which there is no longer an IP pool.  Note: following code depends
	// on this being at the end of the list; otherwise it will suck in allocation
	// blocks that should be reported under other pools.
	if wantAllPools {
		usage = append(usage, &PoolUtilization{
			Name: "orphaned allocation blocks",
			CIDR: net.MustParseNetwork("0.0.0.0/0").IPNet,
		})
	}

	// Read all allocation blocks.
	blocks, err := c.client.List(ctx, model.BlockListOptions{}, "")
	if err != nil {
		return nil, err
	}
	for _, kvp := range blocks.KVPairs {
		b := kvp.Value.(*model.AllocationBlock)
		log.Debugf("Got block: %v", b)

		// Find which pool this block belongs to.
		for _, poolUse := range usage {
			if b.CIDR.IsNetOverlap(poolUse.CIDR) && args.ID == b.ID {
				log.Debugf("Block CIDR %v belongs to pool %v", b.CIDR, poolUse.Name)
				poolUse.Blocks = append(poolUse.Blocks, BlockUtilization{
					CIDR:      b.CIDR.IPNet,
					Capacity:  b.NumAddresses(),
					Available: len(b.Unallocated),
				})
				break
			}
		}
	}
	return usage, nil
}

// getMaxPrefixLen returns maximum block size with given host reserved address range.
func getMaxPrefixLen(version int, attrs *HostReservedAttr) (int, error) {
	if attrs == nil {
		if version == 4 {
			return 32, nil
		} else {
			return 128, nil
		}
	}

	// Caclulate how may addresses we should reserve in the block.
	numOfReserved := (uint)(attrs.StartOfBlock + attrs.EndOfBlock)

	// For example, with windows OS, IPs x.0, x.1, x.2 and x.<bcast> are
	// reserved. so, minimum 4 IPs are needed for network
	// creation. As a result, don't allow a block size
	// 30, 31, 32 (for IPv4) and 126, 127, 128 (for IPv6).
	var maxPrefixLen int
	if numOfReserved > 0 {
		if version == 4 {
			maxPrefixLen = 32 - bits.Len(numOfReserved)
		} else {
			maxPrefixLen = 128 - bits.Len(numOfReserved)
		}
	}

	if maxPrefixLen <= 0 {
		return 0, fmt.Errorf("HostReservedAttr has wrong parameters")
	}

	return maxPrefixLen, nil
}
