//go:build !linux

package siwcstate

import (
	"context"
	"errors"
	"os"
)

var ErrLockBusy = errors.New("SIWC domain lock is held")

type Lock struct {
	file    *os.File
	created bool
}

func AcquireLock(context.Context, Domain) (*Lock, error) {
	return nil, ErrUnsafePath
}

func (*Lock) Release() error { return nil }
