// Package standard links the modules of the standard build.
package standard

import (
	_ "github.com/GoldenDeals/aish-core/modules/agent/loop"
	_ "github.com/GoldenDeals/aish-core/modules/frontend/cli"
	_ "github.com/GoldenDeals/aish-core/modules/policy/ask"
	_ "github.com/GoldenDeals/aish-core/modules/store/jsonl"
	_ "github.com/GoldenDeals/aish-core/modules/tool/fs"
)
