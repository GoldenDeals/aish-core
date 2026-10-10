package pathfmt_test

import (
	"testing"
	"time"

	"github.com/GoldenDeals/aish-core/modules/log/internal/pathfmt"
)

func TestFormat(t *testing.T) {
	at := time.Date(2026, 3, 4, 15, 6, 7, 0, time.UTC)
	got := pathfmt.Format("a/%H-%m-%D_%T_%s_%Y%d%M%S_%%_%q.log", "id", at)
	if want := "a/15-03-2026-03-04_15-06-07_id_20260406" + "07_%_%q.log"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
