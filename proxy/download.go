package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("error reading response from %s: %w", url, err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("error from %s: %s", url, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func downloadTile(ctx context.Context, sth *signedTreeHead, prefix *url.URL, level string, tile uint64) ([]byte, error) {
	if partial := sth.TreeSize - tile*entriesPerTile; partial < entriesPerTile {
		if data, err1 := download(ctx, prefix.JoinPath(formatTilePath(level, tile, partial)).String()); err1 == nil {
			return data, nil
		} else if data, err2 := download(ctx, prefix.JoinPath(formatTilePath(level, tile, 0)).String()); err2 == nil {
			return data, nil
		} else {
			return nil, err1
		}
	}
	return download(ctx, prefix.JoinPath(formatTilePath(level, tile, 0)).String())
}

func formatTilePath(level string, tile uint64, partial uint64) string {
	//path := "tile/" + level + "/" + formatTileIndex(tile)
	path := "tile/8/" + level + "/" + formatTileIndex(tile)
	if partial != 0 {
		path += fmt.Sprintf(".p/%d", partial)
	}
	return path
}

func formatTileIndex(tile uint64) string {
	str := ""
	for {
		rem := tile % 1000
		tile = tile / 1000

		if str == "" {
			str = fmt.Sprintf("%03d", rem)
		} else {
			str = fmt.Sprintf("x%03d/%s", rem, str)
		}

		if tile == 0 {
			break
		}
	}
	return str
}
