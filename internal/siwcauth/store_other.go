//go:build !linux

package siwcauth

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var errLinuxCustodyRequired = errors.New("SIWC credential custody requires Linux")

func OpenStore(string, bool) (*Store, error) { return nil, errLinuxCustodyRequired }
func (*Store) withLock(func() error) error   { return errLinuxCustodyRequired }
func (*Store) HostID(bool) (string, error)   { return "", errLinuxCustodyRequired }
func (*Store) Bindings(PublicRegistration, bool) (string, string, error) {
	return "", "", errLinuxCustodyRequired
}
func (*Store) Save(Registration) error { return errLinuxCustodyRequired }
func (*Store) Load(string) (Registration, error) {
	return Registration{}, errLinuxCustodyRequired
}
func (*Store) SetModel(string, string, []Model) error { return errLinuxCustodyRequired }
func (*Store) WithActiveRegistration(Registration, func() error) error {
	return errLinuxCustodyRequired
}
func (*Store) Registrations() ([]PublicRegistration, error) {
	return nil, errLinuxCustodyRequired
}
func (*Store) findRecord(string, *diskRecord) error { return errLinuxCustodyRequired }
func (*Store) VerifyBindings(string, string) error  { return errLinuxCustodyRequired }
func (*Store) refresh(string, time.Time, func(Registration) (Registration, error)) (Registration, error) {
	return Registration{}, errLinuxCustodyRequired
}
func (*Store) removeTokensLocked(diskRecord) error { return errLinuxCustodyRequired }
func (*Store) RemoveTokens(string) error           { return errLinuxCustodyRequired }

func loginWithEndpoints(context.Context, *Store, *http.Client, Endpoints, string, func(context.Context, string) error) (Registration, error) {
	return Registration{}, errLinuxCustodyRequired
}
func Login(context.Context, *Store, *http.Client, string, func(context.Context, string) error) (Registration, error) {
	return Registration{}, errLinuxCustodyRequired
}
func OpenSystemBrowser(context.Context, string) error { return errLinuxCustodyRequired }
