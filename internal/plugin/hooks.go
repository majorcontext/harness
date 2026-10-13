package plugin

import (
	"github.com/majorcontext/harness/internal/plugin/wire"
	pub "github.com/majorcontext/harness/plugin"
)

func hookMethod(h pub.Hook) string { return wire.HookPrefix + string(h) }
