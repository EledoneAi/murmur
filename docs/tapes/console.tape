# Records docs/assets/console.gif and docs/assets/console.png, the console shown in the README, from the demo
# mode: made-up teammates and agents, no hub needed. Run from the repository root after make build:
#
#   vhs docs/tapes/console.tape

Output docs/assets/console.gif

Set Shell "bash"
Set FontFamily "JetBrains Mono"
Set FontSize 15
Set Width 1400
Set Height 760
Set Padding 24
Set Framerate 24
Set TypingSpeed 70ms
Set Theme { "name": "murmur", "background": "#14151c", "foreground": "#d3d6e2", "cursor": "#d3d6e2", "selection": "#2a2e44", "black": "#14151c", "red": "#f47383", "green": "#b4e06c", "yellow": "#e6c46a", "blue": "#7aa7ff", "magenta": "#b99cff", "cyan": "#4fd6c4", "white": "#d3d6e2", "brightBlack": "#5b6078", "brightRed": "#f47383", "brightGreen": "#b4e06c", "brightYellow": "#e6c46a", "brightBlue": "#7aa7ff", "brightMagenta": "#b99cff", "brightCyan": "#4fd6c4", "brightWhite": "#eef0f8" }

# Use the binary just built, without showing the setup in the recording; murmur plays the demo team.
Hide
Type "export PATH=$PWD/bin:$PATH MURMUR_THEME=dark && alias murmur='murmur demo' && clear"
Enter
Show

Type "murmur"
Sleep 400ms
Enter
Sleep 3.5s
Screenshot docs/assets/console.png

# Walk down the board: the selected agent unfolds what it does now.
Type "j"
Sleep 900ms
Type "j"
Sleep 900ms
Type "j"
Sleep 1.2s

# Next thing that needs you, then everything known about it.
Type "n"
Sleep 1.5s
Type " "
Sleep 3.5s
Escape
Sleep 800ms

# A constellation's conversation.
Type "]"
Sleep 1s
Enter
Sleep 3.5s
Escape
Sleep 800ms

# Only your agents, then the team again.
Type "m"
Sleep 2s
Type "t"
Sleep 1.2s

# The palette: agents, people, repos, files and actions, and the key that does each from the board.
Type ":"
Sleep 600ms
Type "billing"
Sleep 2.5s
Enter
Sleep 2.5s
