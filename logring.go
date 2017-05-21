package blockring

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hexablock/blockring/rpc"
	"github.com/hexablock/blockring/structs"
	"github.com/hexablock/txlog"
	"github.com/ipkg/difuse/utils"
)

type LogTransport interface {
	ProposeTx(loc *structs.Location, tx *txlog.Tx, opts txlog.Options) (*txlog.Meta, error)
	NewTx(loc *structs.Location, key []byte, opts txlog.Options) (*txlog.Tx, *txlog.Meta, error)
	GetTx(loc *structs.Location, hash []byte, opts txlog.Options) (*txlog.Tx, *txlog.Meta, error)
	CommitTx(loc *structs.Location, tx *txlog.Tx, opts txlog.Options) (*txlog.Meta, error)
}

// LogRing is the core interface to perform operations around the ring.
type LogRing struct {
	locator   *locatorRouter
	transport LogTransport

	ch               chan<- *rpc.BlockRPCData // send only channel for block transfer requests
	proxShiftEnabled bool                     // proximity shifting
}

// NewLogRing instantiates an instance.  If the channel is not nil, proximity shifting is
// automatically enabled.
func NewLogRing(locator Locator, trans LogTransport, ch chan<- *rpc.BlockRPCData) *LogRing {

	rs := &LogRing{
		locator:   &locatorRouter{Locator: locator},
		transport: trans,
	}

	if ch != nil {
		rs.ch = ch
		rs.proxShiftEnabled = true
	}

	return rs
}

func (lr *LogRing) NewTx(key []byte, opts txlog.Options) (*txlog.Tx, *txlog.Meta, error) {
	keyHash, _, succs, err := lr.locator.LookupKey(key, 1)
	if err != nil {
		return nil, nil, err
	}
	loc := &structs.Location{Id: keyHash, Vnode: succs[0]}
	return lr.transport.NewTx(loc, key, opts)
}

// ProposeTx proposes a transaction to the network.
func (lr *LogRing) ProposeTx(tx *txlog.Tx, opts txlog.Options) (*txlog.Meta, error) {

	locs, err := lr.locator.LocateReplicatedKey(tx.Key, int(opts.PeerSetSize))
	if err != nil {
		return nil, err
	}

	var (
		wg    sync.WaitGroup
		errCh = make(chan error, len(locs))
		done  = make(chan struct{})
		bail  int32
		meta  *txlog.Meta
	)

	wg.Add(len(locs))

	if opts.Source != nil && len(opts.Source) > 0 {
		// Broadcast to all vnodes skipping the source.
		for _, l := range locs {
			// 1 go-routine per location
			go func(loc *structs.Location) {

				if atomic.LoadInt32(&bail) == 0 {
					if !utils.EqualBytes(loc.Vnode.Id, opts.Source) {
						o := txlog.Options{
							Destination: loc.Vnode.Id,
							Source:      opts.Source,
							PeerSetSize: opts.PeerSetSize,
						}
						if _, er := lr.transport.ProposeTx(loc, tx, o); er != nil {
							errCh <- er
						}

					}
				}
				wg.Done()

			}(l)

		}

	} else {
		// Broadcast to all vnodes
		for _, l := range locs {

			go func(loc *structs.Location) {

				if atomic.LoadInt32(&bail) == 0 {
					o := txlog.Options{
						Destination: loc.Vnode.Id,
						Source:      loc.Vnode.Id,
						PeerSetSize: opts.PeerSetSize,
					}
					if _, er := lr.transport.ProposeTx(loc, tx, o); er != nil {
						errCh <- er
					}
				}

				wg.Done()

			}(l)

		}

	}

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case err = <-errCh:
		atomic.StoreInt32(&bail, 1)
	}

	return meta, err
}

func (lr *LogRing) CommitTx(tx *txlog.Tx, opts txlog.Options) (*txlog.Meta, error) {
	locs, err := lr.locator.LocateReplicatedKey(tx.Key, int(opts.PeerSetSize))
	if err != nil {
		return nil, err
	}

	var meta *txlog.Meta
	if opts.Source != nil && len(opts.Source) > 0 {
		// Broadcast to all vnodes skipping the source.
		for _, loc := range locs {
			if utils.EqualBytes(loc.Vnode.Id, opts.Source) {
				continue
			}

			opts.Destination = loc.Vnode.Id
			//log.Printf("action=commit src=%x dst=%s", opts.Source, utils.ShortVnodeID(loc.Vnode))
			if _, er := lr.transport.CommitTx(loc, tx, opts); er != nil {
				err = er
				break
			}
		}

	} else {
		// Broadcast to all vnodes
		for _, loc := range locs {
			opts.Source = loc.Vnode.Id
			opts.Destination = loc.Vnode.Id
			//log.Printf("action=commit src=%x dst=%s", opts.Source, utils.ShortVnodeID(loc.Vnode))
			if _, er := lr.transport.CommitTx(loc, tx, opts); er != nil {
				err = er
				break
			}
		}

	}

	return meta, err
}

func (lr *LogRing) GetTx(id []byte, opts txlog.Options) (*txlog.Tx, *txlog.Meta, error) {

	var (
		tx   *txlog.Tx
		meta *txlog.Meta
	)

	err := lr.locator.RouteHash(id, int(opts.PeerSetSize), func(l *structs.Location) bool {
		t, m, err := lr.transport.GetTx(l, id, opts)
		if err == nil {
			tx = t
			meta = m
			return false
		}
		return true
	})

	if err == nil {
		if tx == nil {
			err = fmt.Errorf("tx not found")
		}
	}

	return tx, meta, err
}

// EnableProximityShifting enables or disables proximity shifting.  Proximity shifing can only enabled
// if the input block channel is not nil.
func (lr *LogRing) EnableProximityShifting(enable bool) {
	if enable {
		if lr.ch != nil {
			lr.proxShiftEnabled = true
		}
	} else {
		lr.proxShiftEnabled = false
	}
}
