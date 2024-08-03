package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/mod/sumdb/tlog"
	"strconv"
	"strings"
)

type signedTreeHead struct {
	TreeSize          uint64 `json:"tree_size"`
	Timestamp         uint64 `json:"timestamp"`
	SHA256RootHash    []byte `json:"sha256_root_hash"`
	TreeHeadSignature []byte `json:"tree_head_signature"`
}

func (sth *signedTreeHead) tlogTree() tlog.Tree {
	return tlog.Tree{
		N:    int64(sth.TreeSize),
		Hash: tlog.Hash(sth.SHA256RootHash),
	}
}

func chompCheckpointLine(input []byte) (string, []byte, bool) {
	newline := bytes.IndexByte(input, '\n')
	if newline == -1 {
		return "", nil, false
	}
	return string(input[:newline]), input[newline+1:], true
}

func parseCheckpoint(input []byte, logID LogID) (*signedTreeHead, error) {
	// origin
	origin, input, _ := chompCheckpointLine(input)

	// tree size
	sizeLine, input, _ := chompCheckpointLine(input)
	treeSize, err := strconv.ParseUint(sizeLine, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("malformed tree size: %w", err)
	}

	// root hash
	hashLine, input, _ := chompCheckpointLine(input)
	rootHash, err := base64.StdEncoding.DecodeString(hashLine)
	if err != nil {
		return nil, fmt.Errorf("malformed root hash: %w", err)
	}
	if len(rootHash) != merkleHashLen {
		return nil, fmt.Errorf("root hash has wrong length (should be %d bytes long, not %d)", merkleHashLen, len(rootHash))
	}

	// 0 or more non-empty extension lines (ignored)
	for {
		line, rest, ok := chompCheckpointLine(input)
		if !ok {
			return nil, errors.New("signed note ended prematurely")
		}
		input = rest
		if len(line) == 0 {
			break
		}
	}

	// signature lines
	signaturePrefix := "\u2014 " + origin + " "
	keyID := makeKeyID(origin, logID)
	for {
		signatureLine, rest, ok := chompCheckpointLine(input)
		if !ok {
			return nil, errors.New("signed note is missing signature from the log")
		}
		input = rest
		if !strings.HasPrefix(signatureLine, signaturePrefix) {
			continue
		}
		signatureBytes, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(signatureLine, signaturePrefix))
		if err != nil {
			return nil, fmt.Errorf("malformed signature: %w", err)
		}
		if !bytes.HasPrefix(signatureBytes, keyID[:]) {
			continue
		}
		if len(signatureBytes) < 12 {
			return nil, errors.New("malformed signature: too short")
		}
		timestamp := binary.BigEndian.Uint64(signatureBytes[4:12])
		signature := signatureBytes[12:]
		return &signedTreeHead{
			TreeSize:          treeSize,
			Timestamp:         timestamp,
			SHA256RootHash:    rootHash,
			TreeHeadSignature: signature,
		}, nil
	}
}

func makeKeyID(origin string, logID LogID) [4]byte {
	h := sha256.New()
	h.Write([]byte(origin))
	h.Write([]byte{'\n', 0x05})
	h.Write(logID[:])

	var digest [sha256.Size]byte
	h.Sum(digest[:0])
	return [4]byte(digest[:4])
}
