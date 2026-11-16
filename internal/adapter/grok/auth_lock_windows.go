package grok

import "errors"

// The official Windows refresh-lock protocol has not been verified. Do not
// consume a rotating token without sharing the official client's lock.
type AuthRefreshLock struct{}

func AcquireAuthRefreshLock(string) (*AuthRefreshLock, error) {
	return nil, errors.New("grok: live OAuth refresh locking is not supported on Windows")
}
func (*AuthRefreshLock) OwnsPath() bool { return false }
func (*AuthRefreshLock) Close() error   { return nil }
