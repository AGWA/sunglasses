package proxy

import (
	"context"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func download(ctx context.Context, url string) ([]byte, error) {
	numRetries := 0
retry:
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if shouldRetry(ctx, numRetries, nil) {
			numRetries++
			goto retry
		}
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		if shouldRetry(ctx, numRetries, nil) {
			numRetries++
			goto retry
		}
		return nil, fmt.Errorf("error reading response from %s: %w", url, err)
	}
	if resp.StatusCode != 200 {
		if shouldRetry(ctx, numRetries, resp) {
			numRetries++
			goto retry
		}
		return nil, fmt.Errorf("%s from %s: %s", resp.Status, url, strings.TrimSpace(string(body)))
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

func shouldRetry(ctx context.Context, numRetries int, resp *http.Response) bool {
	if numRetries == maxRetries {
		return false
	}
	if resp != nil && !isRetryableStatusCode(resp.StatusCode) {
		return false
	}
	delay := baseRetryDelay * (1 << numRetries)
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	delay += randomDuration(0, delay/2)
	if retryAfter, hasRetryAfter := getRetryAfter(resp); hasRetryAfter && retryAfter > delay {
		delay = retryAfter
	}
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline && time.Now().Add(delay).After(deadline) {
		return false
	}
	sleep(ctx, delay)
	return true
}

func isRetryableStatusCode(code int) bool {
	return code/100 == 5 || code == http.StatusTooManyRequests
}

func randomDuration(min, max time.Duration) time.Duration {
	return min + mathrand.N(max-min+1)
}

func getRetryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	seconds, err := strconv.ParseUint(resp.Header.Get("Retry-After"), 10, 16)
	if err != nil {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func sleep(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

const (
	baseRetryDelay = 1 * time.Second
	maxRetryDelay  = 10 * time.Second
	maxRetries     = 5
)
