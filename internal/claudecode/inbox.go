package claudecode

import (
	"encoding/json"
	"net"
	"time"
)

// inject hands a message to a Claude Code session through its own inbox socket, the channel Claude Code uses
// between sessions. The session's own settings decide whether it is delivered, held for approval or refused.
func Inject(sock, text string) error {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}})
	_, err = c.Write(append(line, '\n'))
	return err
}
