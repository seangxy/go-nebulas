// Copyright (C) 2018 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package sync

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/nebulasio/go-nebulas/util/byteutils"

	"github.com/gogo/protobuf/proto"
	"github.com/nebulasio/go-nebulas/core"
	"github.com/nebulasio/go-nebulas/net"
	"github.com/nebulasio/go-nebulas/net/p2p"
	"github.com/nebulasio/go-nebulas/sync/pb"
	"github.com/nebulasio/go-nebulas/util/logging"
	"github.com/sirupsen/logrus"
)

const (
	chunkDataStatusFinished = int64(-1)
	chunkDataStatusNotStart = int64(0)
)

var (
	ErrInvalidChainChunksMessageData    = errors.New("invalid ChainChunks message data")
	ErrWrongChainChunksMessageData      = errors.New("wrong ChainChunks message data")
	ErrInvalidChainChunkDataMessageData = errors.New("invalid ChainChunkData message data")
	ErrWrongChainChunkDataMessageData   = errors.New("wrong ChainChunkData message data")
	ErrInvalidChunkHeaderSourcePeer     = errors.New("invalid chunk headers source peer")
)

type SyncTask struct {
	quitCh                            chan bool
	statusCh                          chan error
	blockChain                        *core.BlockChain
	syncPointBlock                    *core.Block
	netService                        p2p.Manager
	chunk                             *Chunk
	syncMutex                         sync.Mutex
	chainSyncPeers                    []string
	maxConsistentChunkHeadersCount    int
	maxConsistentChunkHeaders         *syncpb.ChunkHeaders
	allChunkHeaders                   map[string]*syncpb.ChunkHeaders
	chunkHeadersRootHashCounter       map[string]int
	receivedChunkHeadersRootHashPeers map[string]bool

	chainSyncDoneCh               chan bool
	chainChunkDataSyncPosition    int
	chainChunkDataProcessPosition int
	chainChunkData                map[int]*syncpb.ChunkData
	chainChunkDataStatus          map[int]int64
	chinGetChunkDataDoneCh        chan bool

	// debug fields.
	chainSyncRetryCount int
}

func NewSyncTask(blockChain *core.BlockChain, netService p2p.Manager, chunk *Chunk) *SyncTask {
	return &SyncTask{
		quitCh:                            make(chan bool, 1),
		statusCh:                          make(chan error, 1),
		blockChain:                        blockChain,
		syncPointBlock:                    blockChain.TailBlock(),
		netService:                        netService,
		chunk:                             chunk,
		chainSyncPeers:                    nil,
		maxConsistentChunkHeadersCount:    0,
		maxConsistentChunkHeaders:         nil,
		allChunkHeaders:                   make(map[string]*syncpb.ChunkHeaders),
		chunkHeadersRootHashCounter:       make(map[string]int),
		receivedChunkHeadersRootHashPeers: make(map[string]bool),
		chainSyncDoneCh:                   make(chan bool, 1),
		chainChunkDataSyncPosition:        0,
		chainChunkDataProcessPosition:     0,
		chainChunkData:                    make(map[int]*syncpb.ChunkData),
		chainChunkDataStatus:              make(map[int]int64),
		chinGetChunkDataDoneCh:            make(chan bool, 1),
		// debug fields.
		chainSyncRetryCount: 0,
	}
}

func (st *SyncTask) Start() {
	st.startSyncLoop()
}

func (st *SyncTask) Stop() {
	st.quitCh <- true
}

func (st *SyncTask) startSyncLoop() {
	go func() {
		for {
			// start chain sync.
			st.sendChainSync()

			syncTicker := time.NewTicker(30 * time.Second)

		SYNC_STEP_1:
			for {
				select {
				case <-st.quitCh:
					logging.VLog().Debug("Stopping sync loop.")
					return
				case <-syncTicker.C:
					if !st.hasEnoughChunkHeaders() {
						st.reset()
						st.setSyncPointToLastChunk()
						st.sendChainSync()
						continue
					}
				case <-st.chainSyncDoneCh:
					// go to next step.
					logging.VLog().Debug("ChainSync Finished. Move to GetChainData.")
					break SYNC_STEP_1
				}
			}

			// start get chunk data.
			logging.VLog().Debug("Starting GetChainData from peers.")

			st.sendChainGetChunk()

			getChunkTimeoutTicker := time.NewTicker(10 * time.Second)

		SYNC_STEP_2:
			for {
				select {
				case <-st.quitCh:
					logging.VLog().Debug("Stopping sync loop.")
					return
				case <-getChunkTimeoutTicker.C:
					// for the timeout peer, send message again.
					st.checkChainGetChunkTimeout()
				case <-st.chinGetChunkDataDoneCh:
					// finished.
					logging.VLog().Debug("GetChainData Finished.")
					if len(st.maxConsistentChunkHeaders.ChunkHeaders) == 0 {
						st.statusCh <- nil
						return
					}
					st.reset()
					st.setSyncPointToNewTail()
					break SYNC_STEP_2
				}
			}
		}
	}()
}

func (st *SyncTask) reset() {
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	st.chainSyncPeers = nil
	st.maxConsistentChunkHeadersCount = 0
	st.maxConsistentChunkHeaders = nil
	st.allChunkHeaders = make(map[string]*syncpb.ChunkHeaders)
	st.chunkHeadersRootHashCounter = make(map[string]int)
	st.receivedChunkHeadersRootHashPeers = make(map[string]bool)
	st.chainChunkDataStatus = make(map[int]int64)
	st.chainChunkDataSyncPosition = 0
	st.chainChunkDataProcessPosition = 0
	st.chainChunkData = make(map[int]*syncpb.ChunkData)
}

func (st *SyncTask) setSyncPointToNewTail() {
	st.syncPointBlock = st.blockChain.TailBlock()
}

func (st *SyncTask) setSyncPointToLastChunk() {
	if st.chainSyncRetryCount < 2 {
		// for the first retry, keep current tail.
		return
	}

	// the first block height of chunk is 1.
	lastChunkBlockHeight := uint64(1)
	if st.syncPointBlock.Height()+1 > core.ChunkSize {
		lastChunkBlockHeight = st.syncPointBlock.Height() - uint64(core.ChunkSize)
	}

	st.syncPointBlock = st.blockChain.GetBlockOnCanonicalChainByHeight(lastChunkBlockHeight)
}

func (st *SyncTask) sendChainSync() {
	logging.VLog().WithFields(logrus.Fields{
		"syncPointBlockHeight": st.syncPointBlock.Height(),
		"syncPointBlockHash":   st.syncPointBlock.Hash().String(),
	}).Infof("Starting ChainSync at %d times.", st.chainSyncRetryCount)

	st.chainSyncRetryCount++

	chunkSync := &syncpb.Sync{
		TailBlockHash: st.syncPointBlock.Hash(),
	}

	data, err := proto.Marshal(chunkSync)
	if err != nil {
		return
	}

	// send message to peers.
	st.chainSyncPeers = st.netService.SendMessageToPeers(net.ChainSync, data,
		net.MessagePriorityLow, new(p2p.ChainSyncPeersFilter))
}

func (st *SyncTask) processChunkHeaders(message net.Message) {
	// lock.
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	if st.hasEnoughChunkHeaders() {
		return
	}

	// verify the peers.
	if st.chainSyncPeers == nil {
		logging.VLog().WithFields(logrus.Fields{
			"err": ErrInvalidChunkHeaderSourcePeer,
			"pid": message.MessageFrom(),
		}).Debug("Invalid ChainChunkHeaders message source peer, chinSyncPeers is NIL.")
		st.netService.ClosePeer(message.MessageFrom(), ErrInvalidChunkHeaderSourcePeer)
		return
	}

	isValidSourcePeer := false
	for _, prettyID := range st.chainSyncPeers {
		if prettyID == message.MessageFrom() {
			isValidSourcePeer = true
			break
		}
	}

	if isValidSourcePeer == false {
		logging.VLog().WithFields(logrus.Fields{
			"err": ErrInvalidChunkHeaderSourcePeer,
			"pid": message.MessageFrom(),
		}).Debug("Invalid ChainChunkHeaders message source peer.")
		st.netService.ClosePeer(message.MessageFrom(), ErrInvalidChunkHeaderSourcePeer)
		return
	}

	chunkHeaders := new(syncpb.ChunkHeaders)
	if err := proto.Unmarshal(message.Data().([]byte), chunkHeaders); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
			"pid": message.MessageFrom(),
		}).Debug("Invalid ChainChunkHeaders message data.")
		st.netService.ClosePeer(message.MessageFrom(), ErrInvalidChainChunksMessageData)
		return
	}

	// verify data.
	if ok, err := VerifyChunkHeaders(chunkHeaders); ok == false {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
			"pid": message.MessageFrom(),
		}).Debug("Wrong ChainChunkHeaders message data.")
		st.netService.ClosePeer(message.MessageFrom(), ErrWrongChainChunksMessageData)
		return
	}

	rootHash := byteutils.Hex(chunkHeaders.Root)

	hashPeerKey := fmt.Sprintf("%s-%s", rootHash, message.MessageFrom())
	if st.receivedChunkHeadersRootHashPeers[hashPeerKey] == true {
		logging.VLog().WithFields(logrus.Fields{
			"rootHash": rootHash,
			"pid":      message.MessageFrom(),
		}).Debug("Duplicated ChainChunkHeaders message data.")
		return
	}

	count := st.chunkHeadersRootHashCounter[rootHash] + 1
	st.chunkHeadersRootHashCounter[rootHash] = count
	st.receivedChunkHeadersRootHashPeers[hashPeerKey] = true

	isMax := false
	if count > st.maxConsistentChunkHeadersCount {
		isMax = true
		st.maxConsistentChunkHeadersCount = count
		st.maxConsistentChunkHeaders = chunkHeaders
	}

	st.allChunkHeaders[rootHash] = chunkHeaders

	logging.VLog().WithFields(logrus.Fields{
		"rootHash": rootHash,
		"count":    count,
		"isMax":    isMax,
		"pid":      message.MessageFrom(),
	}).Debug("Processed ChainChunkHeaders message data.")

	if st.hasEnoughChunkHeaders() {
		st.chainSyncDoneCh <- true
	}
}

func (st *SyncTask) sendChainGetChunk() {
	// lock.
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	if len(st.maxConsistentChunkHeaders.ChunkHeaders) == 0 {
		logging.VLog().WithFields(logrus.Fields{
			"maxConsistentChunkHeadersCount":    st.maxConsistentChunkHeadersCount,
			"maxConsistentChunkHeadersRootHash": byteutils.Hex(st.maxConsistentChunkHeaders.Root),
			"countOfChunkHeaders":               len(st.maxConsistentChunkHeaders.ChunkHeaders),
		}).Debug("ChunkHeaders is empty, no need to sync.")

		// done.
		st.chinGetChunkDataDoneCh <- true
		return
	}

	currentSyncChunkDataCount := 0
	chainChunkDataSyncPosition := 0
	for i := 0; i < len(st.maxConsistentChunkHeaders.ChunkHeaders) && currentSyncChunkDataCount < ConcurrentSyncChunkDataCount; i++ {
		if st.chainChunkDataStatus[i] == chunkDataStatusNotStart {
			currentSyncChunkDataCount++
			chainChunkDataSyncPosition = i
			st.sendChainGetChunkMessage(i)
		}
	}

	st.chainChunkDataSyncPosition = chainChunkDataSyncPosition
}

func (st *SyncTask) checkChainGetChunkTimeout() {
	// lock.
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	timeoutAtThreshold := time.Now().Unix() - GetChunkDataTimeout

	for i := 0; i < st.chainChunkDataSyncPosition; i++ {
		t := st.chainChunkDataStatus[i]
		if t == chunkDataStatusFinished || t == chunkDataStatusNotStart {
			continue
		}

		if t <= timeoutAtThreshold {
			// timeout, send again.
			st.sendChainGetChunkMessage(i)
		}
	}
}

func (st *SyncTask) sendChainGetChunkMessage(chunkHeaderIndex int) {
	chunkHeader := st.maxConsistentChunkHeaders.ChunkHeaders[chunkHeaderIndex]
	data, err := proto.Marshal(chunkHeader)
	if err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
		}).Warn("Failed to marshal ChunkHeader.")
		return
	}
	st.netService.SendMessageToPeers(net.ChainGetChunk, data, net.MessagePriorityLow, new(p2p.RandomPeerFilter))
	st.chainChunkDataStatus[chunkHeaderIndex] = time.Now().Unix()
}

func (st *SyncTask) processChunkData(message net.Message) {
	chunkData := new(syncpb.ChunkData)
	if err := proto.Unmarshal(message.Data().([]byte), chunkData); err != nil {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
			"pid": message.MessageFrom(),
		}).Debug("Invalid ChainChunkData message data.")
		st.netService.ClosePeer(message.MessageFrom(), ErrInvalidChainChunkDataMessageData)
		return
	}

	// lock.
	st.syncMutex.Lock()
	defer st.syncMutex.Unlock()

	// verify chunk data.
	chunkDataIndex := -1
	var chunkHeader *syncpb.ChunkHeader

	for i := 0; i < len(st.maxConsistentChunkHeaders.ChunkHeaders); i++ {
		chunkHeader = st.maxConsistentChunkHeaders.ChunkHeaders[i]
		if bytes.Compare(chunkHeader.Root, chunkData.Root) == 0 {
			chunkDataIndex = i
			break
		}
	}

	if chunkDataIndex < 0 {
		logging.VLog().WithFields(logrus.Fields{
			"pid": message.MessageFrom(),
		}).Debug("Wrong ChainChunkData message data.")
		st.netService.ClosePeer(message.MessageFrom(), ErrWrongChainChunkDataMessageData)
		return
	}

	if st.chainChunkDataStatus[chunkDataIndex] == chunkDataStatusFinished {
		logging.VLog().WithFields(logrus.Fields{
			"pid": message.MessageFrom(),
		}).Debug("Duplicated ChainChunkData message data.")
		return
	}

	if ok, err := VerifyChunkData(chunkHeader, chunkData); ok == false {
		logging.VLog().WithFields(logrus.Fields{
			"err": err,
			"pid": message.MessageFrom(),
		}).Debug("Wrong ChainChunkData message data, retry.")
		st.netService.ClosePeer(message.MessageFrom(), err)
		st.sendChainGetChunkMessage(chunkDataIndex)
		return
	}

	st.chainChunkData[chunkDataIndex] = chunkData
	chunk, ok := st.chainChunkData[st.chainChunkDataProcessPosition]
	for ok {
		if err := st.chunk.processChunkData(chunk); err != nil {
			logging.VLog().WithFields(logrus.Fields{
				"err": err,
				"pid": message.MessageFrom(),
			}).Debug("Wrong ChainChunkData message data, retry.")
			st.netService.ClosePeer(message.MessageFrom(), err)
			st.sendChainGetChunkMessage(chunkDataIndex)
			return
		}
		st.chainChunkDataProcessPosition++
		chunk, ok = st.chainChunkData[st.chainChunkDataProcessPosition]
	}

	// mark done.
	st.chainChunkDataStatus[chunkDataIndex] = chunkDataStatusFinished

	// sync next chunk.
	logging.VLog().Debugf("Succeed to get chain chunk %d.")
	st.sendChainGetChunkForNext()
}

func (st *SyncTask) sendChainGetChunkForNext() {
	nextPos := st.chainChunkDataSyncPosition + 1
	if nextPos >= len(st.maxConsistentChunkHeaders.ChunkHeaders) {
		if st.hasFinishedGetAllChunkData() {
			st.chinGetChunkDataDoneCh <- true
		}
		return
	}

	st.chainChunkDataSyncPosition = nextPos
	st.sendChainGetChunkMessage(nextPos)
}

func (st *SyncTask) hasEnoughChunkHeaders() bool {
	chainSyncPeersCount := 0
	if st.chainSyncPeers != nil {
		chainSyncPeersCount = len(st.chainSyncPeers)
	}

	ret := chainSyncPeersCount > 0 && st.maxConsistentChunkHeadersCount >= int(math.Sqrt(float64(chainSyncPeersCount)))
	if ret {
		logging.VLog().WithFields(logrus.Fields{
			"chainSyncPeers":                    st.chainSyncPeers,
			"chainSyncPeersCount":               chainSyncPeersCount,
			"chainSyncRetryCount":               st.chainSyncRetryCount,
			"maxConsistentChunkHeadersCount":    st.maxConsistentChunkHeadersCount,
			"maxConsistentChunkHeadersRootHash": byteutils.Hex(st.maxConsistentChunkHeaders.Root),
			"countOfChunkHeaders":               len(st.maxConsistentChunkHeaders.ChunkHeaders),
		}).Debug("Received enough chunk headers.")
	}
	return ret
}

func (st *SyncTask) hasFinishedGetAllChunkData() bool {
	total := len(st.maxConsistentChunkHeaders.ChunkHeaders)
	missing := 0
	for i := 0; i < total; i++ {
		if st.chainChunkDataStatus[i] != chunkDataStatusFinished {
			missing++
		}
	}

	if missing > 0 {
		logging.VLog().WithFields(logrus.Fields{
			"totalSyncingChunkHeaders": total,
			"missingCount":             missing,
		}).Debug("Waiting for ChunkData.")
		return false
	}

	logging.VLog().Debug("Received enough chunk data.")
	return true
}
