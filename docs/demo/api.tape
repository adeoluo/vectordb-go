# Records docs/demo/api.gif from a real run: `vhs docs/demo/api.tape` from the repo root.
Output docs/demo/api.gif
Set Shell "bash"
Set FontSize 15
Set Width 1300
Set Height 480
Set Padding 18
Set Theme "Catppuccin Mocha"
Set TypingSpeed 22ms

Hide
Type `export PS1='$ ' && make build >/dev/null && (./bin/vectordb serve --data-dir $(mktemp -d) --addr 127.0.0.1:18081 >/dev/null 2>&1 &) && sleep 1 && clear` Enter
Sleep 10s
Show
Type `curl -s localhost:18081/v1/collections -d '{"name":"docs","dim":3,"metric":"cosine"}'; echo` Enter
Sleep 1.5s
Type `curl -s localhost:18081/v1/collections/docs/upsert -d '{"vectors":[{"id":"a","values":[0.1,0.9,0]},{"id":"b","values":[0.8,0.1,0.1]},{"id":"c","values":[0.2,0.7,0.1]}]}'; echo` Enter
Sleep 1.5s
Type `curl -s localhost:18081/v1/collections/docs/search -d '{"vector":[0.1,0.8,0.05],"k":2}'; echo` Enter
Sleep 3s
