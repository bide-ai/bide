# The terminal half of assets/crash-demo.gif. Run it through record.sh, which builds bide-demo from
# this checkout and puts it on PATH: the hidden `go` function below runs that build, so the
# recording shows the published command while it plays the code in the tree.
Output "terminal.mp4"

Set Shell "bash"
Set FontSize 20
Set Width 790
Set Height 900
Set Padding 22
Set Theme "Dracula"
Set TypingSpeed 35ms
Set Framerate 24

Hide
Type "go() { bide-demo -no-browser -addr 127.0.0.1:18486; }; PS1='$ '; clear"
Enter
Show

# record.sh assumes Enter lands 2.6s into the recording (400ms + 52 characters at 35ms + 400ms).
Sleep 400ms
Type "go run github.com/bide-ai/bide/cmd/bide-demo@latest"
Sleep 400ms
Enter
Wait+Screen@40s /Ctrl-C to quit/
Sleep 2500ms
