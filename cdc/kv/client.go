package kv

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/cdcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/log"
	pd "github.com/pingcap/pd/client"
	"github.com/pingcap/tidb-cdc/cdc/util"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const (
	dialTimeout = 10 * time.Second
)

type RegionFeedEvent struct {
	Val        *RegionFeedValue
	Checkpoint *RegionnFeedCheckpoint
}

type RegionnFeedCheckpoint struct {
	Span       util.Span
	ResolvedTS uint64
}

type RegionFeedValue struct {
	Key []byte
	// nil for delete
	Value []byte
	TS    uint64
}

type singleRegionInfo struct {
	meta *metapb.Region
	span util.Span
	ts   uint64
}

type CDCClient struct {
	pd pd.Client

	clusterID uint64

	mu struct {
		sync.Mutex
		conns map[string]*grpc.ClientConn
	}

	storeMu struct {
		sync.Mutex
		stores map[uint64]*metapb.Store
	}
}

func NewCDCClient(pd pd.Client) (c *CDCClient, err error) {
	clusterID := pd.GetClusterID(context.Background())
	log.Info("get clusterID", zap.Uint64("id", clusterID))

	c = &CDCClient{
		clusterID: clusterID,
		pd:        pd,
		mu: struct {
			sync.Mutex
			conns map[string]*grpc.ClientConn
		}{
			conns: make(map[string]*grpc.ClientConn),
		},
		storeMu: struct {
			sync.Mutex
			stores map[uint64]*metapb.Store
		}{
			stores: make(map[uint64]*metapb.Store),
		},
	}

	return
}

func (c *CDCClient) Close() error {
	c.mu.Lock()
	for _, conn := range c.mu.conns {
		conn.Close()
	}
	c.mu.Unlock()

	return nil
}

func (c *CDCClient) getConn(ctx context.Context, addr string) (conn *grpc.ClientConn, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if conn, ok := c.mu.conns[addr]; ok {
		return conn, nil
	}

	ctx, cancel := context.WithTimeout(ctx, dialTimeout)

	conn, err = grpc.DialContext(
		ctx,
		addr,
		grpc.WithInsecure(),
		grpc.WithBackoffMaxDelay(time.Second),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	cancel()

	if err != nil {
		return nil, errors.Annotatef(err, "dial %s fail", addr)
	}

	c.mu.conns[addr] = conn

	return
}

func (c *CDCClient) getConnByMeta(ctx context.Context, meta *metapb.Region) (conn *grpc.ClientConn, err error) {
	if len(meta.Peers) == 0 {
		return nil, errors.New("no peer")
	}

	// the first is leader in Peers?
	peer := meta.Peers[0]

	store, err := c.getStore(ctx, peer.GetStoreId())
	if err != nil {
		return nil, err
	}

	return c.getConn(ctx, store.Address)
}

func (c *CDCClient) getStore(ctx context.Context, id uint64) (store *metapb.Store, err error) {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()

	if store, ok := c.storeMu.stores[id]; ok {
		return store, nil
	}

	store, err = c.pd.GetStore(ctx, id)
	if err != nil {
		return nil, errors.Annotatef(err, "get store %d failed", id)
	}

	c.storeMu.stores[id] = store

	return
}

func (c *CDCClient) scanRegions(ctx context.Context, key []byte, limit int) (regions []*metapb.Region, peers []*metapb.Peer, err error) {
	// Will return "unknown method ScanRegions"
	// Look like pd don't impl this ScanRegions really yet
	// return c.pd.ScanRegions(ctx, start, limit)

	for {
		var region *metapb.Region
		var peer *metapb.Peer
		region, peer, err = c.pd.GetRegion(ctx, key)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}

		regions = append(regions, region)
		peers = append(peers, peer)

		if len(regions) == limit {
			return
		}

		key = region.EndKey

		// No more region
		if key == nil {
			return
		}
	}

}

// EventFeed divides a EventFeed request on range boundaries and establishes
// a EventFeed to each of the individual region. It streams back result on the
// provided channel.
func (c *CDCClient) EventFeed(
	ctx context.Context, span util.Span, ts uint64, eventCh chan<- *RegionFeedEvent,
) error {

	g, ctx := errgroup.WithContext(ctx)

	regionCh := make(chan singleRegionInfo, 16)
	g.Go(func() error {
		for {
			select {
			case sri := <-regionCh:
				g.Go(func() error {
					return c.partialRegionFeed(ctx, &sri, regionCh, eventCh)
				})
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	g.Go(func() error {
		return c.divideAndSendEventFeedToRegions(ctx, span, ts, regionCh)
	})

	return g.Wait()
}

// partialRegionFeed establishes a EventFeed to the region specified by regionInfo.
// It manages lifecycle events of the region in order to maintain the EventFeed
// connection, this may involve retry this region EventFeed or subdividing the region
// further in the event of a split.
func (c *CDCClient) partialRegionFeed(
	ctx context.Context,
	regionInfo *singleRegionInfo,
	regionCh chan<- singleRegionInfo,
	eventCh chan<- *RegionFeedEvent,
) error {
	ts := regionInfo.ts

	berr := backoff.Retry(func() error {
		var err error

		if regionInfo.meta == nil {
			regionInfo.meta, _, err = c.pd.GetRegion(ctx, regionInfo.span.Start)
		}

		if err != nil {
			log.Warn("get meta failed", zap.Error(err))
			return err
		}

		maxTS, serr := c.singleEventFeed(ctx, regionInfo.span, regionInfo.ts, regionInfo.meta, eventCh)

		if maxTS > ts {
			ts = maxTS
		}

		if serr != nil {
			log.Info("EventFeed disconnected",
				zap.Reflect("span", regionInfo.span),
				zap.Uint64("checkpoint", ts))

			return backoff.Permanent(serr)
			// TODO
			// retry for some err type
			// switch t := errors.Cause(serr).(type) {
			// default:
			// 	return backoff.Permanent(serr)
			// }
		}

		return nil
	}, backoff.NewExponentialBackOff())

	return errors.Trace(berr)
}

// divideAndSendEventFeedToRegions split up the input span
// into non-overlapping spans aligned to region boundaries.
func (c *CDCClient) divideAndSendEventFeedToRegions(
	ctx context.Context, span util.Span, ts uint64, regionCh chan<- singleRegionInfo,
) error {
	limit := 20

	nextSpan := span

	for {
		regions, _, err := c.scanRegions(ctx, nextSpan.Start, limit)
		if err != nil {
			return errors.Trace(err)
		}

		for _, region := range regions {
			partialSpan, err := util.Intersect(nextSpan, util.Span{Start: region.StartKey, End: region.EndKey})
			if err != nil {
				return errors.Trace(err)
			}

			nextSpan.Start = region.EndKey

			select {
			case regionCh <- singleRegionInfo{
				meta: region,
				span: partialSpan,
				ts:   ts,
			}:
			case <-ctx.Done():
				return ctx.Err()
			}

			if util.EndCompare(nextSpan.Start, span.End) >= 0 {
				return nil
			}
		}
	}

}

// singleEventFeed makes a EventFeed RPC call.
// Results will be send to eventCh
// EventFeed RPC will not return checkpoint event directly
// Checkpoint event is generate while there's not non-match pre-write
// Return the maximum checkpoint
func (c *CDCClient) singleEventFeed(
	ctx context.Context,
	span util.Span,
	ts uint64,
	meta *metapb.Region,
	eventCh chan<- *RegionFeedEvent,
) (checkpointTS uint64, err error) {

	req := &cdcpb.ChangeDataRequest{
		Header: &cdcpb.Header{
			ClusterId: c.clusterID,
		},
		RegionId:     meta.GetId(),
		RegionEpoch:  meta.RegionEpoch,
		CheckpointTs: int64(ts),
		StartKey:     span.Start,
		EndKey:       span.End,
	}

	conn, err := c.getConnByMeta(ctx, meta)
	if err != nil {
		return uint64(req.CheckpointTs), err
	}

	client := cdcpb.NewChangeDataClient(conn)

	notMatchTS := make(map[uint64][]byte)

	for {
		stream, err := client.EventFeed(ctx, req)

		if err != nil {
			log.Error("RPC error", zap.Error(err))
			continue
		}

		var maxRecvHeartbeatTs uint64

		for {
			cevent, err := stream.Recv()
			if err == io.EOF {
				return uint64(req.CheckpointTs), nil
			}

			if err != nil {
				return uint64(req.CheckpointTs), errors.Trace(err)
			}

			log.Debug("recv ChangeDataEvent", zap.Stringer("event", cevent))

			for _, event := range cevent.Events {
				switch x := event.Event.(type) {
				case *cdcpb.Event_Entries_:
					for _, row := range x.Entries.GetEntries() {
						switch row.Type {
						case cdcpb.Event_PREWRITE:
							notMatchTS[row.StartTs] = row.GetValue()
						case cdcpb.Event_COMMIT:

							if row.CommitTs > maxRecvHeartbeatTs {
								maxRecvHeartbeatTs = row.CommitTs
							}

							// emit a value
							value := row.GetValue()
							if pvalue, ok := notMatchTS[row.StartTs]; ok {
								if len(pvalue) > 0 {
									value = pvalue
								}
							}

							delete(notMatchTS, row.StartTs)
							if row.GetOpType() == cdcpb.Event_Row_DELETE {
								value = nil
							}
							revent := &RegionFeedEvent{
								Val: &RegionFeedValue{
									Key:   row.Key,
									Value: value,
									TS:    row.CommitTs,
								},
							}
							select {
							case eventCh <- revent:
							case <-ctx.Done():
								return uint64(req.CheckpointTs), errors.Trace(ctx.Err())
							}
						case cdcpb.Event_ROLLBACK:
							delete(notMatchTS, row.StartTs)
						}
					}
				case *cdcpb.Event_Admin_:
					log.Info("receive admin event", zap.Stringer("event", event))
				case *cdcpb.Event_Error_:
					// x.Error
					// TODO a empty message send by tikv now
					// should add some filed about what error happen
					log.Warn("receive error msg", zap.Stringer("event", event))
				case *cdcpb.Event_HearbeatTs:
					if x.HearbeatTs > maxRecvHeartbeatTs {
						maxRecvHeartbeatTs = x.HearbeatTs
					}
				}
			}

			if len(notMatchTS) == 0 {
				if maxRecvHeartbeatTs > uint64(req.CheckpointTs) {
					req.CheckpointTs = int64(maxRecvHeartbeatTs)
				}

				// emit a checkpoint
				revent := &RegionFeedEvent{
					Checkpoint: &RegionnFeedCheckpoint{
						Span:       span,
						ResolvedTS: maxRecvHeartbeatTs,
					},
				}
				select {
				case eventCh <- revent:
				case <-ctx.Done():
					return uint64(req.CheckpointTs), errors.Trace(ctx.Err())
				}
			}
		}
	}
}
