package daemon

import "testing"

// notify-send's body is markup on most Linux desktops: a teammate's message shows as the text it is, never as a link
// or an image.
func TestNotificationIsNotMarkup(t *testing.T) {
	if got := markupSafe(`<a href="https://x.example">fix</a> & <img src=x>`); got != `&lt;a href="https://x.example"&gt;fix&lt;/a&gt; &amp; &lt;img src=x&gt;` {
		t.Fatalf("got %q", got)
	}
}
