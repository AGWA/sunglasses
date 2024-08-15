package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type downloadError struct {
	Err        error
	StatusCode int
	RetryAfter time.Duration
}

func (e *downloadError) Error() string {
	return e.Err.Error()
}

func (e *downloadError) Unwrap() error {
	return e.Err
}

func download(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &downloadError{Err: err}
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, &downloadError{Err: fmt.Errorf("error reading response from %s: %w", url, err)}
	}
	if resp.StatusCode != 200 {
		return nil, &downloadError{
			Err:        fmt.Errorf("%s from %s: %s", resp.Status, url, strings.TrimSpace(string(body))),
			StatusCode: resp.StatusCode,
			RetryAfter: getRetryAfter(resp),
		}
	}
	return body, nil
}

func downloadRetry(ctx context.Context, url string) ([]byte, error) {
	const (
		baseRetryDelay = 1 * time.Second
		maxRetryDelay  = 10 * time.Second
		maxRetries     = 5
	)

	numRetries := 0
	for {
		var derr *downloadError
		if resp, err := download(ctx, url); err == nil {
			return resp, nil
		} else if !errors.As(err, &derr) {
			return nil, err
		} else if numRetries == maxRetries {
			return nil, fmt.Errorf("%w (retried %d times)", err, numRetries)
		} else if !isRetryableStatusCode(derr.StatusCode) {
			return nil, fmt.Errorf("%w (not retryable)", err)
		}
		delay := baseRetryDelay * (1 << numRetries)
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		delay += randomDuration(0, delay/2)
		if derr.RetryAfter > delay {
			delay = derr.RetryAfter
		}
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline && time.Now().Add(delay).After(deadline) {
			return nil, fmt.Errorf("%w (retried %d times)", derr, numRetries)
		}
		if !sleep(ctx, delay) {
			return nil, fmt.Errorf("%w (retried %d times)", derr, numRetries)
		}
		numRetries++
	}
}

func downloadTile(ctx context.Context, sth *signedTreeHead, prefix *url.URL, level string, tile uint64) ([]byte, error) {
	if partial := sth.TreeSize - tile*entriesPerTile; partial < entriesPerTile {
		if data, err1 := downloadRetry(ctx, prefix.JoinPath(formatTilePath(level, tile, partial)).String()); err1 == nil {
			return data, nil
		} else if data, err2 := downloadRetry(ctx, prefix.JoinPath(formatTilePath(level, tile, entriesPerTile)).String()); err2 == nil {
			return data, nil
		} else {
			return nil, err1
		}
	}
	return downloadRetry(ctx, prefix.JoinPath(formatTilePath(level, tile, entriesPerTile)).String())
}

func formatTilePath(level string, tile uint64, width uint64) string {
	path := "tile/" + level + "/" + formatTileIndex(tile)
	if width != entriesPerTile {
		path += fmt.Sprintf(".p/%d", width)
	}
	return path
}

func formatTileIndex(tile uint64) string {
	const base = 1000
	str := fmt.Sprintf("%03d", tile%base)
	for tile >= base {
		tile = tile / base
		str = fmt.Sprintf("x%03d/%s", tile%base, str)
	}
	return str
}

func isRetryableStatusCode(code int) bool {
	return code/100 == 5 || code == http.StatusTooManyRequests || code == http.StatusBadRequest
}

func randomDuration(min, max time.Duration) time.Duration {
	return min + mathrand.N(max-min+1)
}

func getRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	seconds, err := strconv.ParseUint(resp.Header.Get("Retry-After"), 10, 16)
	if err != nil {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
