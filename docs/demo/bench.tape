# Records docs/demo/bench.gif from a real run: `vhs docs/demo/bench.tape` (after make build).
Output docs/demo/bench.gif
Set Shell "bash"
Set FontSize 15
Set Width 1300
Set Height 560
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 30ms

Hide
Type "export PS1='$ ' && clear" Enter
Show
Type "./bin/vectordb bench -n 20000 -dim 32 -queries 200 -ef 16,64,256 -min-time 300ms" Enter
Sleep 14s
