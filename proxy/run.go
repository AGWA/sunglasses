package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/boltdb/bolt"
	"golang.org/x/sync/errgroup"
	"log"
	"slices"
	"software.sslmate.com/src/certspotter/merkletree"
	"time"
)

func (srv *Server) Run() error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := srv.tick(); err != nil {
			return err
		}
		<-ticker.C
	}
}

type offsetCollapsedTree struct {
	merkletree.CollapsedTree
	Offset uint64
}

// insert newTree into trees such that it is sorted in descending order by Offset
func insertPendingTree(trees []offsetCollapsedTree, newTree offsetCollapsedTree) []offsetCollapsedTree {
	position := len(trees)
	for position > 0 {
		if newTree.Offset < trees[position-1].Offset {
			break
		}
		position--
	}
	return slices.Insert(trees, position, newTree)
}

func (srv *Server) tick() error {
	var position *merkletree.CollapsedTree
	if positionBytes, err := srv.load(stateBucket, positionKey); err != nil {
		return fmt.Errorf("error loading position from database: %w", err)
	} else if positionBytes == nil {
		position = merkletree.EmptyCollapsedTree()
	} else if err := json.Unmarshal(positionBytes, &position); err != nil {
		return fmt.Errorf("error unmarshaling position from database: %w", err)
	}

	sth, err := srv.downloadSTH()
	if err != nil {
		log.Printf("error downloading checkpoint (will try again later): %s", err)
		return nil
	}

	log.Printf("position = %d, latest STH = %d", position.Size(), sth.TreeSize)

	if !(sth.TreeSize > position.Size()) {
		return nil
	}

	firstTile := position.Size() / entriesPerTile
	firstTileSkip := position.Size() % entriesPerTile
	endTile := sth.TreeSize / entriesPerTile
	if sth.TreeSize%entriesPerTile != 0 {
		endTile++
	}
	numTiles := endTile - firstTile

	finishedTrees := make(chan offsetCollapsedTree)
	group, ctx := errgroup.WithContext(context.Background())
	group.SetLimit(100)

	group.Go(func() error {
		position := merkletree.EmptyCollapsedTree()
		var pending []offsetCollapsedTree
		for range numTiles {
			updatedPosition := false
			select {
			case <-ctx.Done():
				return ctx.Err()
			case tree := <-finishedTrees:
				if tree.Offset == position.Size() {
					if err := position.Append(&tree.CollapsedTree); err != nil {
						panic(err)
					}
					updatedPosition = true
				} else {
					pending = insertPendingTree(pending, tree)
				}
			}
			for len(pending) > 0 && pending[len(pending)-1].Offset == position.Size() {
				if err := position.Append(&pending[len(pending)-1].CollapsedTree); err != nil {
					panic(err)
				}
				updatedPosition = true
				pending = pending[:len(pending)-1]
			}
			if updatedPosition {
				if positionBytes, err := json.Marshal(position); err != nil {
					return fmt.Errorf("error marshaling position: %w", err)
				} else if err := srv.store(stateBucket, positionKey, positionBytes); err != nil {
					return fmt.Errorf("error storing position in database: %w", err)
				}
				log.Printf("position is now %d, number of pending trees = %d", position.Size(), len(pending))
			}
		}
		if len(pending) != 0 {
			panic(fmt.Errorf("%d tress still pending after download", len(pending)))
		}
		rootHash := position.CalculateRoot()
		if rootHash != merkletree.Hash(sth.SHA256RootHash) {
			return fmt.Errorf("root hash computed from leaves (%x) doesn't match STH root hash (%x)", rootHash[:], sth.SHA256RootHash[:])
		}
		if sthBytes, err := json.Marshal(sth); err != nil {
			return fmt.Errorf("error marshaling STH: %w", err)
		} else if err := srv.store(stateBucket, sthKey, sthBytes); err != nil {
			return fmt.Errorf("error storing STH in database: %w", err)
		}
		srv.sth.Store(sth)

		log.Printf("updated STH to %d", sth.TreeSize)
		return nil
	})
	group.Go(func() error {
		tree := offsetCollapsedTree{CollapsedTree: *position}
		return srv.indexTile(ctx, sth, firstTile, firstTileSkip, tree, finishedTrees)
	})
	for tile := firstTile + 1; tile < endTile; tile++ {
		group.Go(func() error {
			tree := offsetCollapsedTree{Offset: tile * entriesPerTile}
			return srv.indexTile(ctx, sth, tile, 0, tree, finishedTrees)
		})
	}
	return group.Wait()
}

func (srv *Server) indexTile(ctx context.Context, sth *signedTreeHead, tile uint64, skip uint64, tree offsetCollapsedTree, finishedTrees chan<- offsetCollapsedTree) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	numHashes := min(entriesPerTile, sth.TreeSize-tile*entriesPerTile)

	//log.Printf("downloading and indexing leaf tile %d, expecting %d hashes, of which %d will be skipped...", tile, numHashes, skip)

	data, err := downloadTile(ctx, sth, srv.monitoringPrefix, "0", tile)
	if err != nil {
		return err
	}

	if minLen := numHashes * merkleHashLen; uint64(len(data)) < minLen {
		return fmt.Errorf("server returned %d bytes for tile %d, but we were expecting at least %d", len(data), tile, minLen)
	}

	if err := srv.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(leafBucket)

		for i := skip; i < numHashes; i++ {
			entryIndex := tile*entriesPerTile + i
			hash := data[i*merkleHashLen : (i+1)*merkleHashLen]
			tree.Add(merkletree.Hash(hash))

			var entryIndexBytes [8]byte
			binary.BigEndian.PutUint64(entryIndexBytes[:], entryIndex)
			currentEntryIndexBytes := bucket.Get(hash)
			if currentEntryIndexBytes == nil || bytes.Compare(entryIndexBytes[:], currentEntryIndexBytes) < 0 {
				if err := bucket.Put(hash, entryIndexBytes[:]); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("error during database transaction: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case finishedTrees <- tree:
	}
	return nil
}

func (srv *Server) downloadSTH() (*signedTreeHead, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	checkpointURL := srv.monitoringPrefix.JoinPath("checkpoint")
	checkpointBytes, err := download(ctx, checkpointURL.String())
	if err != nil {
		return nil, err
	}

	// TODO: validate checkpoint signature

	sth, err := parseCheckpoint(checkpointBytes)
	if err != nil {
		return nil, fmt.Errorf("error parsing checkpoint: %w", err)
	}
	return sth, nil
}
