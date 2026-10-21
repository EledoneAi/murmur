package harness

import (
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/protocol"
)

// murmur's send and btw count as messages under every harness's name for them: Claude Code's and Codex's, Gemini
// CLI's, Copilot CLI's, OpenCode's and Cursor's. Another server's tool of the same name doesn't.
func TestMurmurTools(t *testing.T) {
	for tool, doing := range map[string]string{
		"mcp__murmur__send": "murmur · send", "mcp_murmur_send": "murmur · send", "murmur-send": "murmur · send",
		"murmur_send": "murmur · send", "MCP:send": "murmur · send",
		"mcp__murmur__btw": "murmur · btw", "mcp_murmur_btw": "murmur · btw", "murmur-btw": "murmur · btw",
		"murmur_btw": "murmur · btw", "MCP:btw": "murmur · btw",
	} {
		if c, _ := CallOf("/work", tool, []byte(`{"to":"bob/b1","question":"what are you on?"}`)); c.Kind != protocol.KindMessage || c.Doing != doing {
			t.Errorf("%s: %+v", tool, c)
		}
	}
	if c, _ := CallOf("/work", "mcp__other__btw", []byte(`{}`)); c.Kind != 0 || c.Doing != "other · btw" {
		t.Errorf("another server's btw: %+v", c)
	}
}
