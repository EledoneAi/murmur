package claude

import (
	"encoding/json"
	"net"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/harness"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

// MinVersion is the oldest Claude Code that takes messages from other sessions through its inbox.
const MinVersion = "2.1.224"

// TooOld says whether Claude Code version v can't take murmur's messages.
func TooOld(v string) bool { return release.Older(v, MinVersion) }

type tooOld string

func (v tooOld) Error() string {
	return "it runs Claude Code " + string(v) + ", which can't take messages from other sessions; " + MinVersion + " or later can"
}

// Push hands a message to a session through its own inbox socket, the channel Claude Code uses between sessions. The
// session's own settings decide whether it is delivered, held for approval or refused.
func (Claude) Push(s harness.Session, text string) error {
	if s.Version != "" && TooOld(s.Version) {
		return tooOld(s.Version)
	}
	return Inject(s.Socket, text)
}

// Inject writes one message to an inbox socket.
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
