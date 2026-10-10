// Command aish runs an agent session with the modules of the standard build.
package main

import (
	"fmt"
	"os"

	"github.com/GoldenDeals/aish-core/app"
	_ "github.com/GoldenDeals/aish-core/modules/standard"
)

func main() {
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
