# The terminal half of assets/crash-demo.gif. Run it through record.sh, which builds bide from this
# checkout and puts it on PATH: the hidden `bide` function below runs that build with the
# recording's flags, so the recording shows the command the README gives while it plays the code in
# the tree.
Output "terminal.mp4"

Set Shell "bash"
Set FontSize 18
Set Width 790
Set Height 1050
Set Padding 22
Set Theme "Dracula"
Set TypingSpeed 35ms
Set Framerate 24

Hide
Type `bide() { command bide "$@" -no-browser -addr 127.0.0.1:18486; }; PS1='$ '; clear`
Enter
Show

# record.sh assumes Enter lands 1.1s into the recording (400ms + 9 characters at 35ms + 400ms).
Sleep 400ms
Type "bide demo"
Sleep 400ms
Enter
Wait+Screen@90s /Ctrl-C to quit/
Sleep 4s
