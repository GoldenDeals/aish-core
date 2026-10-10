// Package pathfmt expands the placeholders in the file names of the log modules.
package pathfmt

import (
	"strings"
	"time"
)

// Format replaces %Y %m %d %H %M %S with the parts of t, %D with Y-m-d,
// %T with H-M-S (no colons, which file names avoid), %s with the session ID
// and %% with a percent sign. Unknown verbs are kept as written.
func Format(pattern, session string, t time.Time) string {
	return strings.NewReplacer(
		"%%", "%",
		"%Y", t.Format("2006"),
		"%m", t.Format("01"),
		"%d", t.Format("02"),
		"%H", t.Format("15"),
		"%M", t.Format("04"),
		"%S", t.Format("05"),
		"%D", t.Format("2006-01-02"),
		"%T", t.Format("15-04-05"),
		"%s", session,
	).Replace(pattern)
}
