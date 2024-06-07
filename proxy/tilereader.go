package proxy

import (
	"context"
	"golang.org/x/mod/sumdb/tlog"
	"golang.org/x/sync/errgroup"
	"net/url"
)

type tileReader struct {
	ctx    context.Context
	prefix *url.URL
}

func (*tileReader) Height() int {
	return tileHeight
}

func (reader *tileReader) ReadTiles(tiles []tlog.Tile) ([][]byte, error) {
	tileData := make([][]byte, len(tiles))
	group, ctx := errgroup.WithContext(reader.ctx)
	group.SetLimit(100)
	for i := range tiles {
		group.Go(func() error {
			tileURL := reader.prefix.JoinPath(tiles[i].Path())
			if resp, err := download(ctx, tileURL.String()); err != nil {
				return err
			} else {
				tileData[i] = resp
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return tileData, nil
}

func (*tileReader) SaveTiles([]tlog.Tile, [][]byte) {
}
