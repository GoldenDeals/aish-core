package events

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/GoldenDeals/aish-core"
)

func message0(deltas ...ContentDelta) AssistantMessage {
	s, w := aish.NewStream[ContentDelta]()
	for _, d := range deltas {
		w.Write(d)
	}
	w.Close(nil)

	return AssistantMessage{Content: s}
}

func renderBlocks(bs []Block) string {
	out := make([]string, 0, len(bs))
	for i := range bs {
		b := &bs[i]
		switch b.Type {
		case BlockThinking:
			out = append(out, "thinking:"+b.Text+"/"+b.Signature)
		case BlockRedactedThinking:
			out = append(out, "redacted:"+string(b.Data))
		default:
			out = append(out, b.Type+":"+b.Text)
		}
	}

	return strings.Join(out, " ")
}

func TestAssistantMessageBlocks(t *testing.T) {
	t.Parallel()

	text := func(s string) ContentDelta { return ContentDelta{Type: DeltaText, Text: s} }
	think := func(s string) ContentDelta { return ContentDelta{Type: DeltaThinking, Text: s} }
	sign := func(s string) ContentDelta { return ContentDelta{Type: DeltaSignature, Signature: s} }
	stop := ContentDelta{Type: DeltaStop, StopReason: "end_turn"}

	for _, tc := range []struct {
		name   string
		deltas []ContentDelta
		want   string
	}{
		{name: "empty", want: ""},
		{name: "text chunks merge", deltas: []ContentDelta{text("a"), text("b"), stop}, want: "text:ab"},
		{name: "empty text is dropped", deltas: []ContentDelta{text(""), stop}, want: ""},
		{
			name:   "text, thinking, text are three blocks",
			deltas: []ContentDelta{text("a"), think("t"), sign("s"), text("b")},
			want:   "text:a thinking:t/s text:b",
		},
		{
			name:   "two signed thinking blocks stay apart",
			deltas: []ContentDelta{think("1"), sign("s1"), think("2"), sign("s2")},
			want:   "thinking:1/s1 thinking:2/s2",
		},
		{name: "thinking cut off by text is dropped", deltas: []ContentDelta{think("t"), text("a")}, want: "text:a"},
		{name: "thinking cut off at the end is dropped", deltas: []ContentDelta{text("a"), think("t")}, want: "text:a"},
		{name: "signature without thinking is ignored", deltas: []ContentDelta{text("a"), sign("s"), text("b")}, want: "text:ab"},
		{
			name:   "redacted thinking ends the open block",
			deltas: []ContentDelta{text("a"), {Type: DeltaRedactedThinking, Data: []byte("r")}, text("b")},
			want:   "text:a redacted:r text:b",
		},
		{name: "unknown chunk types carry nothing", deltas: []ContentDelta{{Type: "citation", Text: "x"}, text("a")}, want: "text:a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderBlocks(message0(tc.deltas...).Blocks()); got != tc.want {
				t.Errorf("Blocks = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAssistantMessageTextAndStop(t *testing.T) {
	t.Parallel()

	m := message0(
		ContentDelta{Type: DeltaThinking, Text: "hidden"},
		ContentDelta{Type: DeltaText, Text: "Hi"},
		ContentDelta{Type: DeltaStop, StopReason: "end_turn", Usage: &Usage{OutputTokens: 2}},
	)
	if m.Text() != "Hi" {
		t.Errorf("Text = %q, want only text deltas", m.Text())
	}
	if stop, ok := m.Stop(); !ok || stop.StopReason != "end_turn" || stop.Usage.OutputTokens != 2 {
		t.Errorf("Stop = %+v, %v", stop, ok)
	}

	unfinished := message0(ContentDelta{Type: DeltaStop}, ContentDelta{Type: DeltaText, Text: "late"})
	if _, ok := unfinished.Stop(); ok {
		t.Error("Stop found a stop chunk that is not the last one")
	}
	if _, ok := (AssistantMessage{}).Stop(); ok || (AssistantMessage{}).Text() != "" {
		t.Error("zero AssistantMessage has a stop or text")
	}
}

func TestLLMRequestSectionsAndTools(t *testing.T) {
	t.Parallel()

	var r LLMRequest
	base := r.Section("base")
	base.Text = "You are aish."
	r.Section("skills").Text = "skills"
	if again := r.Section("base"); again != base || again.Text != "You are aish." {
		t.Error("Section returned a new section for an existing name")
	}
	for range 10 {
		r.Section("x") // growing the list must not invalidate earlier pointers
	}
	base.Text = "changed"
	if r.System[0].Text != "changed" || len(r.System) != 3 {
		t.Errorf("System = %+v, want base, skills, x with base changed through its pointer", r.System)
	}

	r.AppendTool(ToolSpec{Name: "b"})
	r.AppendTool(ToolSpec{Name: "a"})
	names := []string{r.Tools[0].Name, r.Tools[1].Name}
	if !slices.Equal(names, []string{"b", "a"}) {
		t.Errorf("tools = %v, want the order of AppendTool", names)
	}
}

func TestAssistantMessageMarshalFlattensChunks(t *testing.T) {
	content, w := aish.NewStream[ContentDelta]()
	for _, c := range []ContentDelta{
		{Type: DeltaText, Text: "Hel"},
		{Type: DeltaText, Text: "lo"},
		{Type: DeltaStop, StopReason: "end_turn"},
	} {
		w.Write(c)
	}
	w.Close(nil)

	data, err := json.Marshal(AssistantMessage{Provider: "p", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	var back AssistantMessage
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	body := back.Content.Body()
	if len(body) != 2 || body[0].Text != "Hello" || body[1].StopReason != "end_turn" {
		t.Fatalf("body = %+v, json = %s", body, data)
	}
}

func TestAssistantMessageStoredAsObject(t *testing.T) {
	roundTrip := func(body ...ContentDelta) (string, []ContentDelta) {
		content, w := aish.NewStream[ContentDelta]()
		for _, c := range body {
			w.Write(c)
		}
		w.Close(nil)
		data, err := json.Marshal(AssistantMessage{Provider: "p", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		var back AssistantMessage
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}

		return string(data), back.Content.Body()
	}

	data, body := roundTrip(ContentDelta{Type: DeltaText, Text: "a"}, ContentDelta{Type: DeltaText, Text: "b"},
		ContentDelta{Type: DeltaStop, StopReason: "end_turn", Usage: &Usage{OutputTokens: 2}})
	if want := `{"Provider":"p","Model":"","Content":{"Text":"ab","StopReason":"end_turn","Usage":{"InputTokens":0,"OutputTokens":2,"CacheReadTokens":0,"CacheWriteTokens":0}}}`; data != want {
		t.Fatalf("json = %s", data)
	}
	if len(body) != 2 || body[0].Text != "ab" {
		t.Fatalf("body = %+v", body)
	}

	_, body = roundTrip(ContentDelta{Type: DeltaThinking, Text: "t"}, ContentDelta{Type: DeltaSignature, Signature: "s"},
		ContentDelta{Type: DeltaText, Text: "x"}, ContentDelta{Type: DeltaToolUse, ToolID: "1"})
	if len(body) != 4 || body[2].Text != "x" || body[3].ToolID != "1" {
		t.Fatalf("order lost: %+v", body)
	}

	var old AssistantMessage
	if err := json.Unmarshal([]byte(`{"Content":[{"Type":"text","Text":"o"}]}`), &old); err != nil || old.Content.Body()[0].Text != "o" {
		t.Fatalf("array form: %v", err)
	}
}
