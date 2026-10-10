package aish_test

import (
	"encoding/json"
	"testing"

	"github.com/GoldenDeals/aish-core"
)

type codecA struct{ N int }

func (codecA) Kind() string { return "codec.test" }

type codecB struct{}

func (codecB) Kind() string { return "codec.test" }

type codecStream struct{ S aish.Stream[string] }

func (codecStream) Kind() string { return "codec.stream" }

func TestRegisterEventDecode(t *testing.T) {
	aish.RegisterEvent[codecA]()
	aish.RegisterEvent[codecA]()

	ev, ok, err := aish.DecodeEvent("codec.test", json.RawMessage(`{"N":7}`))
	if err != nil || !ok || ev.(codecA).N != 7 {
		t.Fatalf("got %v %v %v", ev, ok, err)
	}
	if _, ok, _ := aish.DecodeEvent("codec.unknown", nil); ok {
		t.Fatal("unknown kind decoded")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("no panic for second type with the same kind")
		}
	}()
	aish.RegisterEvent[codecB]()
}

func TestStreamJSONRoundTrip(t *testing.T) {
	aish.RegisterEvent[codecStream]()
	s, w := aish.NewStream[string]()
	w.Write("a")
	w.Write("b")
	w.Close(nil)

	raw, err := json.Marshal(codecStream{S: s})
	if err != nil {
		t.Fatal(err)
	}
	ev, _, err := aish.DecodeEvent("codec.stream", raw)
	if err != nil {
		t.Fatal(err)
	}
	got := ev.(codecStream).S
	<-got.Done()
	if b := got.Body(); len(b) != 2 || b[0] != "a" || b[1] != "b" {
		t.Fatalf("body %v", b)
	}
}

type ptrEvent struct{ N int }

func (ptrEvent) Kind() string { return "codec.ptr" }

func TestRegisterEventPointerType(t *testing.T) {
	aish.RegisterEvent[*ptrEvent]()

	ev, ok, err := aish.DecodeEvent("codec.ptr", json.RawMessage(`{"N":3}`))
	if err != nil || !ok || ev.(*ptrEvent).N != 3 {
		t.Fatalf("got %v %v %v", ev, ok, err)
	}
}
