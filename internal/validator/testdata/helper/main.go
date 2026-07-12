package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/yourikka/minicloud/internal/validator/protocol"
)

func main() {
	request, _, err := protocol.ReadRequest(os.Stdin, protocol.HardArtifactBytes)
	if err != nil {
		os.Exit(2)
	}

	switch request.ValidationID {
	case "sleep", "sleep-one", "sleep-two":
		time.Sleep(10 * time.Second)
	case "invalid-report":
		_, _ = fmt.Fprint(os.Stdout, "{}")
	case "output-limit":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte{'x'}, protocol.MaxReportBytes+1))
	case "crash":
		_, _ = fmt.Fprintln(os.Stderr, "secret guest path: /private/artifact.wasm")
		os.Exit(1)
	default:
		os.Exit(3)
	}
}
