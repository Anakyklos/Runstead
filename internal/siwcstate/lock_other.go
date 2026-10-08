//go:build !linux

package siwcstate

import (
	"context"
	"errors"
)

var ErrLockBusy = errors.New("SIWC domain lock is held")

type Lock struct{}

func AcquireLock(context.Context, Domain) (*Lock, error) {
	return nil, ErrUnsafePath
}

func (*Lock) Release() error { return nil }
