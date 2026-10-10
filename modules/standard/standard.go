// Package standard links the modules of the standard build.
package standard

import (
	_ "github.com/GoldenDeals/aish-core/modules/agent/loop"
	_ "github.com/GoldenDeals/aish-core/modules/context/agents_md"
	_ "github.com/GoldenDeals/aish-core/modules/context/skills"
	_ "github.com/GoldenDeals/aish-core/modules/context/system_prompt"
	_ "github.com/GoldenDeals/aish-core/modules/frontend/cli"
	_ "github.com/GoldenDeals/aish-core/modules/llm/anthropic"
	_ "github.com/GoldenDeals/aish-core/modules/log/file"
	_ "github.com/GoldenDeals/aish-core/modules/log/trace"
	_ "github.com/GoldenDeals/aish-core/modules/policy/ask"
	_ "github.com/GoldenDeals/aish-core/modules/policy/cedar"
	_ "github.com/GoldenDeals/aish-core/modules/store/jsonl"
	_ "github.com/GoldenDeals/aish-core/modules/tool/bash"
	_ "github.com/GoldenDeals/aish-core/modules/tool/fs"
	_ "github.com/GoldenDeals/aish-core/modules/tool/mcp"
)
