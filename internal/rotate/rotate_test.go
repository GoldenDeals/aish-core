package rotate_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/GoldenDeals/aish-core/internal/rotate"
)

type buf struct{ bytes.Buffer }

func (*buf) Close() error { return nil }

func TestRotatesOnlyWhenTheNameChanges(t *testing.T) {
	files := map[string]*buf{}
	next := "a"
	w, err := rotate.Open(&rotate.Writer{
		Period: time.Millisecond,
		Name:   func(time.Time) string { return next },
		Create: func(name string) (io.WriteCloser, error) { files[name] = &buf{}; return files[name], nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	_, _ = w.Write([]byte("1")) // period passed, same name: stays
	next = "b"
	time.Sleep(5 * time.Millisecond)
	_, _ = w.Write([]byte("2"))
	if files["a"].String() != "1" || files["b"].String() != "2" {
		t.Fatalf("a=%q b=%q", files["a"], files["b"])
	}
}
