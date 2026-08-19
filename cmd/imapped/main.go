// Command imapped is an IMAP caching proxy with a web interface.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/esaiaswestberg/imapped/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().ExecuteContext(context.Background()); err != nil {
		// Configuration errors are multi-line by design; print them verbatim so
		// every problem is visible at once rather than one restart at a time.
		fmt.Fprintf(os.Stderr, "imapped: %v\n", err)
		os.Exit(1)
	}
}
