package proxy

import (
	"bytes"
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

func chompCheckpointLine(input []byte) (string, []byte) {
	newline := bytes.IndexByte(input, '\n')
	if newline == -1 {
		return "", nil
	}
	return string(input[:newline]), input[newline+1:]
}

func parseCheckpoint(input []byte) (*signedTreeHead, error) {
	preamble, input := chompCheckpointLine(input)
	sizeLine, input := chompCheckpointLine(input)
	hashLine, input := chompCheckpointLine(input)
	blankLine, input := chompCheckpointLine(input)
	signatureLine, input := chompCheckpointLine(input)

	if len(input) != 0 {
		return nil, fmt.Errorf("%d unexpected bytes at end of signed note", len(input))
	}

	treeSize, err := strconv.ParseUint(sizeLine, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("malformed tree size: %w", err)
	}
	rootHash, err := base64.StdEncoding.DecodeString(hashLine)
	if err != nil {
		return nil, fmt.Errorf("malformed root hash: %w", err)
	}
	if len(rootHash) != merkleHashLen {
		return nil, fmt.Errorf("root hash has wrong length (should be %d bytes long, not %d)", merkleHashLen, len(rootHash))
	}
	if len(blankLine) != 0 {
		return nil, errors.New("missing blank line in signed note")
	}
	signaturePrefix := "\u2014 " + preamble + " "
	if !strings.HasPrefix(signatureLine, signaturePrefix) {
		return nil, errors.New("signature line has unexpected prefix")
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(signatureLine, signaturePrefix))
	if err != nil {
		return nil, fmt.Errorf("malformed signature: %w", err)
	}
	if len(signatureBytes) < 12 {
		return nil, fmt.Errorf("malformed signature: too short", err)
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
