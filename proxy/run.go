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

type leafHashes struct {
	startIndex uint64
	hashes     [][]byte
}

func (srv *Server) tick() error {
	sth, err := srv.downloadSTH()
	if err != nil {
		return logContactError{fmt.Errorf("error downloading latest checkpoint: %w", err)}
	}

	if srv.disableLeafIndex {
		if sthBytes, err := json.Marshal(sth); err != nil {
			return fmt.Errorf("error marshaling STH: %w", err)
		} else if err := srv.store(stateBucket, sthKey, sthBytes); err != nil {
			return fmt.Errorf("error storing STH in database: %w", err)
		}
		srv.sth.Store(sth)
		return nil
	}

	var position merkletree.FragmentedCollapsedTree
	if positionBytes, err := srv.load(stateBucket, positionKey); err != nil {
		return fmt.Errorf("error loading position from database: %w", err)
	} else if positionBytes == nil {
	} else if err := json.Unmarshal(positionBytes, &position); err != nil {
		return fmt.Errorf("error unmarshaling position from database: %w", err)
	}

	if position.IsComplete(sth.TreeSize) {
		return nil
	}

	log.Printf("Downloaded STH with tree size %d", sth.TreeSize)

	const workers = 500
	results := make(chan leafHashes, workers)
	group, ctx := errgroup.WithContext(context.Background())
	group.SetLimit(1 + workers)
	group.Go(func() error {
		tx, err := srv.db.Begin(true)
		if err != nil {
			return fmt.Errorf("error starting database transaction: %w", err)
		}
		defer func() { tx.Rollback() }()
		uncommitted := 0
		for ctx.Err() == nil && !position.IsComplete(sth.TreeSize) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case hashes := <-results:
				if err := srv.processLeafHashes(tx, &position, hashes); err != nil {
					return fmt.Errorf("error processing leaf hashes at %d: %w", hashes.startIndex, err)
				}
				uncommitted++
				if uncommitted == 10 {
					if err := commit(tx, position); err != nil {
						return err
					}
					if newTx, err := srv.db.Begin(true); err != nil {
						return fmt.Errorf("error starting database transaction: %w", err)
					} else {
						tx = newTx
					}
					uncommitted = 0
				}
			}
		}
		if err := commit(tx, position); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rootHash := position.Subtree(0).CalculateRoot(); rootHash != merkletree.Hash(sth.SHA256RootHash) {
			return fmt.Errorf("root hash computed from leaves (%x) doesn't match STH root hash (%x)", rootHash[:], sth.SHA256RootHash[:])
		}
		if sthBytes, err := json.Marshal(sth); err != nil {
			return fmt.Errorf("error marshaling STH: %w", err)
		} else if err := srv.store(stateBucket, sthKey, sthBytes); err != nil {
			return fmt.Errorf("error storing STH in database: %w", err)
		}
		srv.sth.Store(sth)

		log.Printf("All entries indexed, updated STH to tree size %d", sth.TreeSize)
		return nil
	})
	startTime := time.Now()
	var numEntries uint64
	//for begin, end := range position.Gaps {
	position.Gaps(func(begin, end uint64) bool {
		if ctx.Err() != nil {
			//break
			return false
		}
		if end == 0 {
			end = sth.TreeSize
		}
		numEntries += end - begin
		log.Printf("Indexing entries in range [%d, %d)...", begin, end)
		for ctx.Err() == nil && begin < end {
			tile := begin / entriesPerTile
			skip := begin % entriesPerTile
			count := min(entriesPerTile-skip, end-begin)
			begin += count

			group.Go(func() error {
				return srv.downloadLeafHashes(ctx, sth, tile, skip, count, results)
			})
		}
		return true
	})

	if err := group.Wait(); err != nil {
		return err
	}
	timeElapsed := time.Since(startTime)
	log.Printf("Indexed %d entries in %s (%f entries per second)", numEntries, timeElapsed, float64(numEntries)/timeElapsed.Seconds())
	return nil
}

func (srv *Server) downloadLeafHashes(ctx context.Context, sth *signedTreeHead, tile uint64, skip uint64, count uint64, results chan<- leafHashes) error {
	data, err := downloadTile(ctx, sth, srv.monitoringPrefix, "0", tile)
	if err != nil {
		return logContactError{fmt.Errorf("error downloading leaf tile %d: %w", tile, err)}
	}
	if minLen := (skip + count) * merkletree.HashLen; uint64(len(data)) < minLen {
		return logContactError{fmt.Errorf("server returned %d bytes for tile %d, but we were expecting at least %d", len(data), tile, minLen)}
	}
	data = data[skip*merkletree.HashLen:]

	hashes := make([][]byte, count)
	for i := range count {
		hashes[i] = data[i*merkleHashLen : (i+1)*merkleHashLen]
	}
	select {
	case <-ctx.Done():
		return logContactError{ctx.Err()}
	case results <- leafHashes{startIndex: tile*entriesPerTile + skip, hashes: hashes}:
		return nil
	}
}

func (srv *Server) processLeafHashes(tx *bolt.Tx, position *merkletree.FragmentedCollapsedTree, hashes leafHashes) error {
	//start := time.Now()
	//defer func() { log.Printf("processed leaf hashes from %d in %s", hashes.startIndex, time.Since(start)) }()

	bucket := tx.Bucket(leafBucket)

	entryIndex := hashes.startIndex
	for _, hash := range hashes.hashes {
		if err := position.AddHash(entryIndex, merkletree.Hash(hash)); err != nil {
			panic(err)
		}
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
}

func (srv *Server) downloadSTH() (*signedTreeHead, error) {
	checkpointURL := srv.monitoringPrefix.JoinPath("checkpoint")
	checkpointBytes, err := downloadRetry(context.Background(), checkpointURL.String())
	if err != nil {
		return nil, err
	}

	sth, err := parseCheckpoint(checkpointBytes)
	if err != nil {
		return nil, fmt.Errorf("error parsing checkpoint: %w", err)
	}
	return sth, nil
}

func commit(tx *bolt.Tx, position merkletree.FragmentedCollapsedTree) error {
	//log.Printf("committing...")
	//start := time.Now()
	if positionBytes, err := json.Marshal(position); err != nil {
		return fmt.Errorf("error marshaling position: %w", err)
	} else if err := tx.Bucket(stateBucket).Put(positionKey, positionBytes); err != nil {
		return fmt.Errorf("error storing position in database: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("error committing transaction: %w", err)
	}
	//log.Printf("committed transaction in %s", time.Since(start))
	return nil
}
