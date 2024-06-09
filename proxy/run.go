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

type logContactError struct {
	error
}

func (e logContactError) Unwrap() error {
	return e.error
}

func isLogContactError(e error) bool {
	_, ok := e.(logContactError)
	return ok
}

func (srv *Server) Run() error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := srv.tick(); isLogContactError(err) {
			log.Printf("error contacting log (will try again later): %s", err)
		} else if err != nil {
			return err
		}
		<-ticker.C
	}
}

type offsetCollapsedTree struct {
	merkletree.CollapsedTree
	Offset uint64
}

type downloadLeavesJob struct {
	tile   uint64
	skip   uint64
	tree   offsetCollapsedTree
	hashes [][]byte
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
		return logContactError{fmt.Errorf("error downloading latest checkpoint: %w", err)}
	}

	if !(sth.TreeSize > position.Size()) {
		return nil
	}

	log.Printf("New STH with tree size %d; indexing entries from %d...", sth.TreeSize, position.Size())

	firstTile := position.Size() / entriesPerTile
	firstTileSkip := position.Size() % entriesPerTile
	endTile := sth.TreeSize / entriesPerTile
	if sth.TreeSize%entriesPerTile != 0 {
		endTile++
	}
	numTiles := endTile - firstTile

	finishedJobs := make(chan *downloadLeavesJob)
	group, ctx := errgroup.WithContext(context.Background())
	group.SetLimit(100)

	group.Go(func() error {
		position := merkletree.EmptyCollapsedTree()
		var pending []offsetCollapsedTree
		for range numTiles {
			var job *downloadLeavesJob
			select {
			case <-ctx.Done():
				return ctx.Err()
			case job = <-finishedJobs:
			}
			if err := srv.indexLeaves(job.tile*entriesPerTile+job.skip, job.hashes); err != nil {
				return fmt.Errorf("error indexing leaves: %w", err)
			}
			updatedPosition := false
			if job.tree.Offset == position.Size() {
				if err := position.Append(&job.tree.CollapsedTree); err != nil {
					panic(err)
				}
				updatedPosition = true
			} else {
				pending = insertPendingTree(pending, job.tree)
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
				log.Printf("Indexed all entries up to tree size %d", position.Size())
			}
		}
		if len(pending) != 0 {
			panic(fmt.Errorf("%d trees still pending after download", len(pending)))
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

		log.Printf("Updated STH to tree size %d", sth.TreeSize)
		return nil
	})
	group.Go(func() error {
		job := &downloadLeavesJob{
			tile: firstTile,
			skip: firstTileSkip,
			tree: offsetCollapsedTree{CollapsedTree: *position},
		}
		return srv.downloadLeaves(ctx, sth, job, finishedJobs)
	})
	for tile := firstTile + 1; ctx.Err() == nil && tile < endTile; tile++ {
		group.Go(func() error {
			job := &downloadLeavesJob{
				tile: tile,
				tree: offsetCollapsedTree{Offset: tile * entriesPerTile},
			}
			return srv.downloadLeaves(ctx, sth, job, finishedJobs)
		})
	}
	return group.Wait()
}

func (srv *Server) downloadLeaves(ctx context.Context, sth *signedTreeHead, job *downloadLeavesJob, finishedJobs chan<- *downloadLeavesJob) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	numHashes := min(entriesPerTile, sth.TreeSize-job.tile*entriesPerTile)

	data, err := downloadTile(ctx, sth, srv.monitoringPrefix, "0", job.tile)
	if err != nil {
		return logContactError{fmt.Errorf("error downloading leaf tile %d: %w", job.tile, err)}
	}
	if minLen := numHashes * merkleHashLen; uint64(len(data)) < minLen {
		return logContactError{fmt.Errorf("server returned %d bytes for tile %d, but we were expecting at least %d", len(data), job.tile, minLen)}
	}

	data = data[job.skip*merkleHashLen:]

	job.hashes = make([][]byte, numHashes-job.skip)
	for i := range job.hashes {
		job.hashes[i] = data[i*merkleHashLen : (i+1)*merkleHashLen]
		job.tree.Add(merkletree.Hash(job.hashes[i]))
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case finishedJobs <- job:
	}
	return nil
}

func (srv *Server) indexLeaves(entryIndex uint64, hashes [][]byte) error {
	return srv.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(leafBucket)

		for _, hash := range hashes {
			var entryIndexBytes [8]byte
			binary.BigEndian.PutUint64(entryIndexBytes[:], entryIndex)
			currentEntryIndexBytes := bucket.Get(hash)
			if currentEntryIndexBytes == nil || bytes.Compare(entryIndexBytes[:], currentEntryIndexBytes) < 0 {
				if err := bucket.Put(hash, entryIndexBytes[:]); err != nil {
					return err
				}
			}
			entryIndex++
		}
		return nil
	})
}

func (srv *Server) downloadSTH() (*signedTreeHead, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	checkpointURL := srv.monitoringPrefix.JoinPath("checkpoint")
	checkpointBytes, err := download(ctx, checkpointURL.String())
	if err != nil {
		return nil, err
	}

	sth, err := parseCheckpoint(checkpointBytes)
	if err != nil {
		return nil, fmt.Errorf("error parsing checkpoint: %w", err)
	}
	return sth, nil
}
