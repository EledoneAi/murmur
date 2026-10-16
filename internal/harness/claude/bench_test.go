package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/harness"
)

// What reading a conversation's transcript costs: the whole of a long one, as when the daemon starts or finds a
// session, and one tool call and its result appended, as each change does: go test ./internal/harness/claude -run
// '^$' -bench . -benchmem

// longTranscript is a conversation of mb megabytes as Claude Code writes it: commands, their output, file writes, a
// tool result far over 64 KB now and then, and a title.
func longTranscript(b *testing.B, mb int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "t.jsonl")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, `{"type":"ai-title","aiTitle":"Rerun billing on staging","sessionId":"s"}`)
	small, big, huge := strings.Repeat("x", 2000), strings.Repeat("y", 30_000), strings.Repeat("z", 200_000)
	for i, written := 0, 0; written < mb<<20; i++ {
		var line string
		switch i % 4 {
		case 0:
			line = answerLine(toolBlock("Bash", `{"command":"go test ./billing/ -run Rerun","description":"`+small+`"}`), 90_000)
		case 1:
			line = `{"parentUuid":"p","message":{"role":"user","content":[{"type":"tool_result","content":"` + big + `"}]},"type":"user"}` + "\n"
		case 2:
			line = answerLine(toolBlock("Write", `{"file_path":"/work/api/billing/rerun.go","content":"`+big+`"}`), 90_000)
		default:
			result := small
			if i%40 == 3 {
				result = huge
			}
			line = `{"parentUuid":"p","message":{"role":"user","content":[{"type":"tool_result","content":"` + result + `"}]},"type":"user"}` + "\n"
		}
		n, _ := f.WriteString(line)
		written += n
	}
	return path
}

func BenchmarkTranscript(b *testing.B) {
	for _, mb := range []int{10, 100} {
		b.Run(fmt.Sprintf("whole-%dMB", mb), func(b *testing.B) {
			path := longTranscript(b, mb)
			b.SetBytes(int64(mb << 20))
			b.ReportAllocs()
			for b.Loop() {
				var a harness.Activity
				(&Transcript{Path: path}).Read(&a)
			}
		})
	}
	b.Run("appended", func(b *testing.B) {
		path := longTranscript(b, 10)
		t, a := &Transcript{Path: path}, harness.Activity{}
		t.Read(&a)
		call := answerLine(toolBlock("Edit", `{"file_path":"/work/api/billing/rerun.go","old_string":"a","new_string":"b"}`), 90_000)
		result := `{"parentUuid":"p","message":{"role":"user","content":[{"type":"tool_result","content":"` + strings.Repeat("r", 3000) + `"}]},"type":"user"}` + "\n"
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			b.Fatal(err)
		}
		defer f.Close()
		b.ReportAllocs()
		for b.Loop() {
			_, _ = f.WriteString(call + result)
			t.Read(&a)
		}
	})
}
