// Command vectordb runs the vector database server and its benchmark tool.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/adeoluo/vectordb-go/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
