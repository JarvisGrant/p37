//go:build !linux

package main

import (
	"errors"
	"time"
)

func mountPayload(_, _ string, _ time.Duration) error {
	return errors.New("p37-mount 仅支持 Linux")
}
