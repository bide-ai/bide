# The terminal half of assets/crash-demo.gif. Run scripts through record.sh, which builds
# bide-demo from this checkout and puts it on PATH: the hidden `go` function below runs that
# build, so the recording shows the published command while it plays the code in the tree.
Output "crash-demo-terminal.mp4"

Set Shell "bash"
Set FontSize 18
Set Width 1100
Set Height 760
Set Padding 24
Set Theme "Dracula"
Set TypingSpeed 35ms
Set Framerate 24

Hide
Type "go() { bide-demo -no-browser -addr 127.0.0.1:18486; }; PS1='$ '; clear"
Enter
Show

Sleep 400ms
Type "go run github.com/bide-ai/bide/cmd/bide-demo@latest"
Sleep 400ms
Enter
Wait+Screen@40s /Ctrl-C to quit/
Sleep 2s
