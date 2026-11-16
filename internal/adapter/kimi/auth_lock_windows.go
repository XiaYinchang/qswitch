package kimi

import (
	"context"
	"errors"
)

// Fail closed until the official Windows refresh-lock protocol is verified.
type AuthRefreshLock struct{}

func AcquireAuthRefreshLock(context.Context, string) (*AuthRefreshLock, error) {
	return nil, errors.New("kimi: live OAuth refresh locking is not supported on Windows")
}
func (*AuthRefreshLock) Context() context.Context { return context.Background() }
func (*AuthRefreshLock) OwnsPath() bool           { return false }
func (*AuthRefreshLock) Close() error             { return nil }
