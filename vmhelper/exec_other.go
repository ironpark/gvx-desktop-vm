//go:build !darwin && !linux

package vmhelper

import "io"

func setupExecTTY(_ io.Reader, _ io.Writer) (rows, cols uint16, watchResize func(send func(r, c uint16)) func(), restore func()) {
	return 0, 0, nil, func() {}
}
