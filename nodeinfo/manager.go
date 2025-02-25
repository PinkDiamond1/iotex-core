// Copyright (c) 2022 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package nodeinfo

import (
	"context"
	"time"

	"github.com/iotexproject/go-pkgs/cache/lru"
	"github.com/iotexproject/go-pkgs/crypto"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/exp/slices"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/blockchain/genesis"
	"github.com/iotexproject/iotex-core/pkg/lifecycle"
	"github.com/iotexproject/iotex-core/pkg/log"
	"github.com/iotexproject/iotex-core/pkg/routine"
	"github.com/iotexproject/iotex-core/pkg/util/byteutil"
	"github.com/iotexproject/iotex-core/pkg/version"
	"github.com/iotexproject/iotex-core/state"
)

type (
	transmitter interface {
		BroadcastOutbound(context.Context, proto.Message) error
		UnicastOutbound(context.Context, peer.AddrInfo, proto.Message) error
		Info() (peer.AddrInfo, error)
	}

	chain interface {
		TipHeight() uint64
		Genesis() genesis.Genesis
	}

	delegatesGetFunc func(context.Context) (state.CandidateList, error)

	// Info node infomation
	Info struct {
		Version   string
		Height    uint64
		Timestamp time.Time
		Address   string
		PeerID    string
	}

	// InfoManager manage delegate node info
	InfoManager struct {
		lifecycle.Lifecycle
		version       string
		address       string
		delegateCache bool
		nodeMap       *lru.Cache
		transmitter   transmitter
		chain         chain
		privKey       crypto.PrivateKey
		getDelegates  delegatesGetFunc
	}
)

var _nodeInfoHeightGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "iotex_node_info_height_gauge",
		Help: "height info of node",
	},
	[]string{"address", "version"},
)

func init() {
	prometheus.MustRegister(_nodeInfoHeightGauge)
}

// NewInfoManager new info manager
func NewInfoManager(cfg *Config, t transmitter, ch chain, privKey crypto.PrivateKey, getDelegatesHandler delegatesGetFunc) *InfoManager {
	dm := &InfoManager{
		nodeMap:      lru.New(cfg.NodeMapSize),
		transmitter:  t,
		chain:        ch,
		privKey:      privKey,
		version:      version.PackageVersion,
		address:      privKey.PublicKey().Address().String(),
		getDelegates: getDelegatesHandler,
	}
	// init recurring tasks
	broadcastTask := routine.NewRecurringTask(func() {
		ctx := protocol.WithFeatureCtx(
			protocol.WithBlockCtx(
				genesis.WithGenesisContext(context.Background(), dm.chain.Genesis()),
				protocol.BlockCtx{BlockHeight: dm.chain.TipHeight()},
			),
		)
		if !protocol.MustGetFeatureCtx(ctx).EnableNodeInfo {
			log.L().Debug("nodeinfo manager feature is disabled")
			return
		}

		// delegates or nodes who are turned on will broadcast
		if cfg.EnableBroadcastNodeInfo || dm.isDelegate() {
			if err := dm.BroadcastNodeInfo(context.Background()); err != nil {
				log.L().Error("nodeinfo manager broadcast node info failed", zap.Error(err))
			}
		} else {
			log.L().Debug("nodeinfo manager general node disabled node info broadcast")
		}
	}, cfg.BroadcastNodeInfoInterval)
	dm.Add(broadcastTask)
	return dm
}

// Start start delegate broadcast task
func (dm *InfoManager) Start(ctx context.Context) error {
	return dm.OnStart(ctx)
}

// Stop stop delegate broadcast task
func (dm *InfoManager) Stop(ctx context.Context) error {
	return dm.OnStop(ctx)
}

// HandleNodeInfo handle node info message
func (dm *InfoManager) HandleNodeInfo(ctx context.Context, peerID string, msg *iotextypes.NodeInfo) {
	log.L().Debug("nodeinfo manager handle node info")
	// recover pubkey
	hash := hashNodeInfo(msg.Info)
	pubKey, err := crypto.RecoverPubkey(hash[:], msg.Signature)
	if err != nil {
		log.L().Warn("nodeinfo manager recover pubkey failed", zap.Error(err))
		return
	}
	// verify signature
	if addr := pubKey.Address().String(); addr != msg.Info.Address {
		log.L().Warn("nodeinfo manager node info message verify failed", zap.String("expected", addr), zap.String("recieved", msg.Info.Address))
		return
	}

	dm.updateNode(&Info{
		Version:   msg.Info.Version,
		Height:    msg.Info.Height,
		Timestamp: msg.Info.Timestamp.AsTime(),
		Address:   msg.Info.Address,
		PeerID:    peerID,
	})
}

// updateNode update node info
func (dm *InfoManager) updateNode(node *Info) {
	addr := node.Address
	// update dm.nodeMap
	dm.nodeMap.Add(addr, *node)
	// update metric
	_nodeInfoHeightGauge.WithLabelValues(addr, node.Version).Set(float64(node.Height))
}

// GetNodeByAddr get node info by address
func (dm *InfoManager) GetNodeByAddr(addr string) (Info, bool) {
	info, ok := dm.nodeMap.Get(addr)
	if !ok {
		return Info{}, false
	}
	return info.(Info), true
}

// BroadcastNodeInfo broadcast request node info message
func (dm *InfoManager) BroadcastNodeInfo(ctx context.Context) error {
	log.L().Debug("nodeinfo manager broadcast node info")
	req, err := dm.genNodeInfoMsg()
	if err != nil {
		return err
	}
	// broadcast request meesage
	if err := dm.transmitter.BroadcastOutbound(ctx, req); err != nil {
		return err
	}
	// manually update self node info for broadcast message to myself will be ignored
	peer, err := dm.transmitter.Info()
	if err != nil {
		return err
	}
	dm.updateNode(&Info{
		Version:   req.Info.Version,
		Height:    req.Info.Height,
		Timestamp: req.Info.Timestamp.AsTime(),
		Address:   req.Info.Address,
		PeerID:    peer.ID.Pretty(),
	})
	return nil
}

// RequestSingleNodeInfoAsync unicast request node info message
func (dm *InfoManager) RequestSingleNodeInfoAsync(ctx context.Context, peer peer.AddrInfo) error {
	log.L().Debug("nodeinfo manager request one node info", zap.String("peer", peer.ID.Pretty()))
	return dm.transmitter.UnicastOutbound(ctx, peer, &iotextypes.NodeInfoRequest{})
}

// HandleNodeInfoRequest tell node info to peer
func (dm *InfoManager) HandleNodeInfoRequest(ctx context.Context, peer peer.AddrInfo) error {
	log.L().Debug("nodeinfo manager tell node info", zap.Any("peer", peer.ID.Pretty()))
	req, err := dm.genNodeInfoMsg()
	if err != nil {
		return err
	}
	return dm.transmitter.UnicastOutbound(ctx, peer, req)
}

func (dm *InfoManager) genNodeInfoMsg() (*iotextypes.NodeInfo, error) {
	req := &iotextypes.NodeInfo{
		Info: &iotextypes.NodeInfoCore{
			Version:   dm.version,
			Height:    dm.chain.TipHeight(),
			Timestamp: timestamppb.Now(),
			Address:   dm.address,
		},
	}
	// add sig for msg
	h := hashNodeInfo(req.Info)
	sig, err := dm.privKey.Sign(h[:])
	if err != nil {
		return nil, errors.Wrap(err, "sign node info message failed")
	}
	req.Signature = sig
	return req, nil
}

func (dm *InfoManager) isDelegate() bool {
	if !dm.delegateCache {
		if err := dm.updateDelegateCache(); err != nil {
			log.L().Error("nodeinfo manager update delegate cache failed", zap.Error(err))
		}
	}
	return dm.delegateCache
}

func (dm *InfoManager) updateDelegateCache() error {
	candList, err := dm.getDelegates(context.Background())
	if err != nil {
		return err
	}
	log.L().Debug("nodeinfo manager active candidates", zap.Any("candidates", candList))
	dm.delegateCache = slices.ContainsFunc(candList, func(e *state.Candidate) bool {
		return dm.address == e.Address
	})
	return nil
}

func hashNodeInfo(msg *iotextypes.NodeInfoCore) hash.Hash256 {
	return hash.Hash256b(byteutil.Must(proto.Marshal(msg)))
}
