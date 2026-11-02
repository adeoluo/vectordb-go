# Records docs/demo/cosine.gif from a real run: `vhs docs/demo/cosine.tape` from the repo root.
Output docs/demo/cosine.gif
Set Shell "bash"
Set FontSize 15
Set Width 1300
Set Height 440
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 22ms

Hide
Type `export PS1='$ ' && make build >/dev/null` Enter
Sleep 2s
Sleep 8s
Type `clear` Enter
Sleep 1s
Show
Type `./bin/vectordb bench -n 8000 -dim 64 -metric cosine -queries 200 -ef 32,128 -min-time 300ms` Enter
Sleep 14s
