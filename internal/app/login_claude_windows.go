package app

import (
	"context"
	"errors"
	"qswitch/internal/adapter"
)

func (*App) LoginClaude(context.Context, func(string)) (adapter.Identity, error) {
	return adapter.Identity{}, errors.New("claude: isolated login is supported on Linux; Windows is not supported")
}
