package proxy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/sync/errgroup"
)

type getEntriesItem struct {
	LeafInput []byte `json:"leaf_input"`
	ExtraData []byte `json:"extra_data"`
}

type entry struct {
	timestampedEntry []byte
	precertificate   []byte // nil iff certificate entry; non-nil iff precertificate entry
	chain            [][32]byte
}

func decodeUint40(buf [5]byte) (i uint64) {
	i |= uint64(buf[0]) << 32
	i |= uint64(buf[1]) << 24
	i |= uint64(buf[2]) << 16
	i |= uint64(buf[3]) << 8
	i |= uint64(buf[4]) << 0
	return
}

func (e *entry) parse(input []byte, leafIndex uint64) ([]byte, error) {
	var skipped cryptobyte.String
	str := cryptobyte.String(input)

	// TimestampedEntry.timestamp
	if !str.Skip(8) {
		return nil, fmt.Errorf("error reading timestamp")
	}
	// TimestampedEntry.entry_type
	var entryType uint16
	if !str.ReadUint16(&entryType) {
		return nil, fmt.Errorf("error reading entry type")
	}
	// TimestampedEntry.signed_entry
	if entryType == 0 {
		if !str.ReadUint24LengthPrefixed(&skipped) {
			return nil, fmt.Errorf("error reading certificate")
		}
	} else if entryType == 1 {
		if !str.Skip(32) {
			return nil, fmt.Errorf("error reading issuer_key_hash")
		}
		if !str.ReadUint24LengthPrefixed(&skipped) {
			return nil, fmt.Errorf("error reading tbs_certificate")
		}
	} else {
		return nil, fmt.Errorf("invalid entry type %d", entryType)
	}

	// TimestampedEntry.extensions
	var extensions cryptobyte.String
	if !str.ReadUint16LengthPrefixed(&extensions) {
		return nil, fmt.Errorf("error reading extensions")
	}
	var hasIndexExtension bool
	for !extensions.Empty() {
		var extType uint8
		var extData cryptobyte.String
		if !extensions.ReadUint8(&extType) {
			return nil, fmt.Errorf("error reading extension type")
		}
		if !extensions.ReadUint16LengthPrefixed(&extData) {
			return nil, fmt.Errorf("error reading extension data")
		}
		if extType != 0 {
			continue
		}
		if hasIndexExtension {
			return nil, fmt.Errorf("duplicate LeafIndex extension")
		}
		if len(extData) != 5 {
			return nil, fmt.Errorf("LeafIndex extension has wrong length")
		}
		if thisIndex := decodeUint40(([5]byte)(extData)); thisIndex != leafIndex {
			return nil, fmt.Errorf("incorrect leaf index %d", thisIndex)
		}
		hasIndexExtension = true
	}
	if !hasIndexExtension {
		return nil, fmt.Errorf("missing LeafIndex extension")
	}

	timestampedEntryLen := len(input) - len(str)
	e.timestampedEntry = input[:timestampedEntryLen]

	// precertificate
	if entryType == 1 {
		var precertificate cryptobyte.String
		if !str.ReadUint24LengthPrefixed(&precertificate) {
			return nil, fmt.Errorf("error reading precertificate")
		}
		e.precertificate = precertificate
	} else {
		e.precertificate = nil
	}

	// certificate_chain
	var chainBytes cryptobyte.String
	if !str.ReadUint16LengthPrefixed(&chainBytes) {
		return nil, fmt.Errorf("error reading certificate_chain")
	}
	e.chain = make([][32]byte, 0, len(chainBytes)/32)
	for !chainBytes.Empty() {
		var fingerprint [32]byte
		if !chainBytes.CopyBytes(fingerprint[:]) {
			return nil, fmt.Errorf("error reading fingerprint in certificate_chain")
		}
		e.chain = append(e.chain, fingerprint)
	}

	return str, nil
}

func (e *entry) leafInput() []byte {
	return append([]byte{0, 0}, e.timestampedEntry...)
}

func (e *entry) extraData(issuers map[[32]byte]*[]byte) []byte {
	b := cryptobyte.NewBuilder(nil)
	if e.precertificate != nil {
		b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddBytes(e.precertificate)
		})
	}
	b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
		for _, fingerprint := range e.chain {
			b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
				b.AddBytes(*issuers[fingerprint])
			})
		}
	})
	return b.BytesOrPanic()
}

func (srv *Server) downloadEntries(ctx context.Context, sth *signedTreeHead, beginIncl, endExcl uint64) ([]getEntriesItem, error) {
	tile := beginIncl / entriesPerTile
	skip := beginIncl % entriesPerTile
	numEntries := min(entriesPerTile, endExcl-tile*entriesPerTile) - skip

	data, err := downloadTile(ctx, sth, srv.monitoringPrefix, "data", tile)
	if err != nil {
		return nil, err
	}
	var skippedEntry entry
	for i := range skip {
		leafIndex := tile*entriesPerTile + i
		if rest, err := skippedEntry.parse(data, leafIndex); err != nil {
			return nil, fmt.Errorf("error parsing entry %d: %w", leafIndex, err)
		} else {
			data = rest
		}
	}
	issuers := make(map[[32]byte]*[]byte)
	entries := make([]entry, numEntries)
	for i := range numEntries {
		leafIndex := tile*entriesPerTile + skip + i
		if rest, err := entries[i].parse(data, leafIndex); err != nil {
			return nil, fmt.Errorf("error parsing entry %d: %w", leafIndex, err)
		} else {
			data = rest
		}
		for _, issuer := range entries[i].chain {
			if _, exists := issuers[issuer]; !exists {
				issuers[issuer] = new([]byte)
			}
		}
	}

	if err := srv.getIssuers(ctx, issuers); err != nil {
		return nil, err
	}

	items := make([]getEntriesItem, numEntries)
	for i := range numEntries {
		items[i].LeafInput = entries[i].leafInput()
		items[i].ExtraData = entries[i].extraData(issuers)
	}
	return items, nil
}

func (srv *Server) getIssuers(ctx context.Context, issuers map[[32]byte]*[]byte) error {
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(100)
	for fingerprint, issuerPtr := range issuers {
		group.Go(func() error {
			issuer, err := srv.getIssuer(ctx, fingerprint)
			if err != nil {
				return fmt.Errorf("error getting issuer %x: %w", fingerprint, err)
			}
			*issuerPtr = issuer
			return nil
		})
	}
	return group.Wait()
}

func (srv *Server) getIssuer(ctx context.Context, fingerprint [32]byte) ([]byte, error) {
	var data []byte
	if err := srv.db.QueryRowContext(ctx, `SELECT data FROM issuer WHERE sha256 = $1`, fingerprint[:]).Scan(&data); err == nil {
		return data, nil
	} else if err != sql.ErrNoRows {
		return nil, fmt.Errorf("error loading issuer from database: %w", err)
	}
	issuerURL := srv.monitoringPrefix.JoinPath("issuer", hex.EncodeToString(fingerprint[:]))
	data, err := downloadRetry(ctx, issuerURL.String())
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(data) != fingerprint {
		return nil, fmt.Errorf("response from %s does not match the fingerprint", issuerURL)
	}
	if _, err := srv.db.ExecContext(ctx, `INSERT INTO issuer (sha256, data) VALUES ($1, $2) ON CONFLICT (sha256) DO NOTHING`, fingerprint[:], data); err != nil {
		return nil, fmt.Errorf("error storing issuer in databaes: %w", err)
	}
	return data, nil
}
