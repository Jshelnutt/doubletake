//go:build !windows

package airplay

import (
	"context"
	"fmt"
	"io"
)

func startWASAPILoopback(ctx context.Context) (io.ReadCloser, <-chan error, error) {
	return nil, nil, fmt.Errorf("WASAPI loopback is only available on Windows")
}
