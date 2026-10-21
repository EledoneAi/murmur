package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/ipc"
)

// teamDaemon answers as the daemon does: who lists the team, send refuses a message without a recipient or text and
// takes the rest. It returns the sends it got.
func teamDaemon(t *testing.T) func() []ipc.Request {
	t.Helper()
	f := fakeDaemon(t, func(req ipc.Request) *ipc.Reply {
		switch req.Op {
		case "who":
			return &ipc.Reply{OK: true, Text: "alice, you\nbob"}
		case "send":
			if strings.TrimSpace(req.To) == "" || strings.TrimSpace(req.Text) == "" {
				return &ipc.Reply{Err: "both `to` and `text` are needed"}
			}
		}
		return &ipc.Reply{OK: true}
	})
	return func() []ipc.Request {
		return slices.DeleteFunc(f.requests(), func(r ipc.Request) bool { return r.Op != "send" })
	}
}

// onPipes runs the server on pipes for its stdin and stdout: write sends it lines, read takes one line of its answer,
// or "" when none comes within a second, and stop closes its stdin and says whether it returned.
func onPipes(t *testing.T) (write func(string), read func() string, stop func() bool) {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	returned := make(chan error, 1)
	go func() { returned <- serve("none", inR, outW) }()
	lines := make(chan string, 100)
	go func() {
		r := bufio.NewReaderSize(outR, 1<<16)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimSuffix(l, "\n")
		}
	}()
	t.Cleanup(func() { inW.Close(); outW.Close(); inR.Close(); outR.Close() })
	write = func(s string) {
		if _, err := io.WriteString(inW, s); err != nil {
			t.Fatal(err)
		}
	}
	read = func() string {
		select {
		case l := <-lines:
			return l
		case <-time.After(time.Second):
			return ""
		}
	}
	stop = func() bool {
		inW.Close()
		select {
		case err := <-returned:
			return err == nil
		case <-time.After(2 * time.Second):
			return false
		}
	}
	return write, read, stop
}

type answer struct {
	ID     json.RawMessage `json:"id"`
	Result struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	} `json:"result"`
	Error *struct{ Code int } `json:"error"`
}

func parse(t *testing.T, line string) answer {
	t.Helper()
	var a answer
	if err := json.Unmarshal([]byte(line), &a); err != nil {
		t.Fatalf("not JSON: %q", line)
	}
	return a
}

// Every request gets one answer under its own id, whatever the id is; notifications, lines that aren't JSON and
// half a message at the end get none, and the server stops when its input closes.
func TestServeAnswersRequestsOnly(t *testing.T) {
	teamDaemon(t)
	write, read, stop := onPipes(t)
	write(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	write("not json\n\n")
	for _, id := range []string{`1`, `"a-1"`, `null`, `{"k":[1]}`, `0`} {
		write(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}` + "\n")
		if a := parse(t, read()); string(a.ID) != id || a.Error != nil {
			t.Fatalf("ping %s: answered %+v", id, a)
		}
	}
	write(`{"jsonrpc":"2.0","id":7,"method":"resources/list"}` + "\n")
	if a := parse(t, read()); a.Error == nil || a.Error.Code != -32601 {
		t.Fatalf("an unknown method: %+v", a)
	}
	write(`{"jsonrpc":"2.0","id":8,"method":"ping"`)
	if !stop() {
		t.Fatal("the server doesn't stop when its input closes")
	}
	if l := read(); l != "" {
		t.Fatalf("half a message got %q", l)
	}
}

// A tool call with no arguments, arguments of the wrong type, an unknown tool, or a recipient or text that is empty
// or blank is an error the agent reads, never a broken answer; a long message reaches the daemon whole.
func TestToolCallsWithBadArguments(t *testing.T) {
	sent := teamDaemon(t)
	write, read, stop := onPipes(t)
	defer stop()
	call := func(params string) answer {
		t.Helper()
		write(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + params + "}\n")
		return parse(t, read())
	}
	for _, params := range []string{`{"name":"send"}`, `{"name":"send","arguments":{"to":5,"text":["x"]}}`,
		`{"name":"send","arguments":{"to":"  ","text":"hi"}}`, `{"name":"send","arguments":{"to":"bob","text":"\n\t "}}`,
		`{"name":"delete"}`, `{}`, `"send"`, `null`} {
		if a := call(params); !a.Result.IsError || len(a.Result.Content) != 1 || a.Result.Content[0].Text == "" {
			t.Errorf("%s: answered %+v", params, a)
		}
	}
	if a := call(`{"name":"who","arguments":{"to":1}}`); a.Result.IsError || !strings.Contains(a.Result.Content[0].Text, "bob") {
		t.Errorf("who with stray arguments: %+v", a)
	}
	long := strings.Repeat("é", 300_000) // a line far past the server's buffer
	if a := call(`{"name":"send","arguments":{"to":"bob","text":"` + long + `"}}`); a.Result.IsError {
		t.Errorf("a long message: %+v", a)
	}
	if got := sent(); len(got) == 0 || got[len(got)-1].Text != long {
		t.Error("the long message didn't reach the daemon whole")
	}
}

// A batch of requests, which MCP took out in 2025-06-18, gets an error rather than no answer, so the client doesn't
// wait for one; the server answers what comes next.
func TestBatchIsRefused(t *testing.T) {
	teamDaemon(t)
	write, read, _ := onPipes(t)
	write(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]` + "\n")
	if a := parse(t, read()); a.Error == nil || a.Error.Code != -32600 {
		t.Fatalf("a batch: %+v", a)
	}
	write(`{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n")
	if a := parse(t, read()); string(a.ID) != "3" || a.Error != nil {
		t.Fatalf("after a batch: %+v", a)
	}
}
